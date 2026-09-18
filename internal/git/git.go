package git

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DiffSummary contains insertions and deletions count
type DiffSummary struct {
	Insertions int
	Deletions  int
}

// Operations handles Git commands
type Operations struct {
	PushRetries           int
	PushRetryDelaySeconds int
	Token                 string // GIT_TOKEN — injected into HTTPS URLs at runtime
}

// NewOperations creates a new Git operations handler
func NewOperations(pushRetries, pushRetryDelaySeconds int) *Operations {
	return &Operations{
		PushRetries:           pushRetries,
		PushRetryDelaySeconds: pushRetryDelaySeconds,
	}
}

// FetchAndReset fetches from origin and resets to origin/main
func (o *Operations) FetchAndReset(ctx context.Context, repoPath string) error {
	fetchTarget := o.resolveRemote(ctx, repoPath)
	if err := o.runGitCommand(ctx, repoPath, "fetch", fetchTarget); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}

	// Get default branch name
	branch, err := o.getDefaultBranch(ctx, repoPath)
	if err != nil {
		branch = "main" // fallback
	}

	// Reset to origin/branch
	if err := o.runGitCommand(ctx, repoPath, "reset", "--hard", "origin/"+branch); err != nil {
		return fmt.Errorf("git reset failed: %w", err)
	}

	// Clean untracked files
	if err := o.runGitCommand(ctx, repoPath, "clean", "-fdx"); err != nil {
		return fmt.Errorf("git clean failed: %w", err)
	}

	return nil
}

// GetChangedFiles returns a list of changed files (staged and unstaged)
func (o *Operations) GetChangedFiles(ctx context.Context, repoPath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "HEAD")
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		// No changes case - check if working tree is clean
		statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
		statusCmd.Dir = repoPath
		statusOutput, _ := statusCmd.Output()
		if len(statusOutput) == 0 {
			return []string{}, nil
		}
		return nil, fmt.Errorf("git diff failed: %w", err)
	}

	files := strings.Split(strings.TrimSpace(string(output)), "\n")
	// Filter empty strings
	result := make([]string, 0, len(files))
	for _, f := range files {
		if f != "" {
			result = append(result, f)
		}
	}

	// Also get untracked files
	statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	statusCmd.Dir = repoPath
	statusOutput, err := statusCmd.Output()
	if err == nil {
		lines := strings.Split(string(statusOutput), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "??") {
				// Untracked file
				file := strings.TrimPrefix(line, "?? ")
				file = strings.TrimSpace(file)
				if file != "" {
					result = append(result, file)
				}
			}
		}
	}

	return result, nil
}

// GetDiffSummary returns the insertions and deletions count
func (o *Operations) GetDiffSummary(ctx context.Context, repoPath string) (DiffSummary, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--stat", "HEAD")
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return DiffSummary{}, nil // No changes is not an error
	}

	return parseDiffStat(string(output)), nil
}

// Commit stages all changes and creates a commit
func (o *Operations) Commit(ctx context.Context, repoPath, message, author, instruction string) (string, error) {
	// Stage all changes
	if err := o.runGitCommand(ctx, repoPath, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add failed: %w", err)
	}

	// Check if there are changes to commit
	statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	statusCmd.Dir = repoPath
	statusOutput, _ := statusCmd.Output()
	if len(strings.TrimSpace(string(statusOutput))) == 0 {
		return "", fmt.Errorf("no changes to commit")
	}

	// Build commit command
	commitAuthor := author
	if commitAuthor == "" {
		commitAuthor = "agent-runner"
	}
	args := []string{"commit", "-m", message, "--author", fmt.Sprintf("%s <bot@local>", commitAuthor)}
	if instruction != "" {
		args = append(args, "--trailer", fmt.Sprintf("Instruction: %s", instruction))
	}

	// Inject committer identity via env so git never needs a global user config.
	// --author sets the author; GIT_COMMITTER_* sets the committer.
	if err := o.runGitCommandEnv(ctx, repoPath, []string{
		"GIT_COMMITTER_NAME=" + commitAuthor,
		"GIT_COMMITTER_EMAIL=bot@local",
	}, args...); err != nil {
		return "", fmt.Errorf("git commit failed: %w", err)
	}

	// Get commit hash
	hashCmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	hashCmd.Dir = repoPath
	hashOutput, err := hashCmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get commit hash: %w", err)
	}

	return strings.TrimSpace(string(hashOutput)), nil
}

// resolveRemote returns the push/fetch target for repoPath. When Token is set
// and the stored remote is an HTTPS URL, it returns an ephemeral URL with the
// token embedded. Otherwise it returns "origin" so normal git credential
// helpers remain in effect.
func (o *Operations) resolveRemote(ctx context.Context, repoPath string) string {
	if o.Token == "" {
		return "origin"
	}
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "origin"
	}
	remote := strings.TrimSpace(string(out))
	if injected := injectToken(remote, o.Token); injected != remote {
		return injected
	}
	return "origin"
}

// injectToken rewrites an HTTPS URL to embed the token as credentials.
// Returns remote unchanged for SSH URLs, when token is empty, or when the
// URL already carries credentials — double-injecting produces a mangled URL.
func injectToken(remote, token string) string {
	if token == "" || remote == "" {
		return remote
	}
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(remote, prefix) {
			rest := remote[len(prefix):]
			if strings.Contains(rest, "@") {
				return remote // credentials already present
			}
			return prefix + "oauth2:" + token + "@" + rest
		}
	}
	return remote
}

// RebaseConflictError reports that integrating the remote's new commits
// stopped on merge conflicts. The rebase is left in progress — conflict
// markers in the working tree, REBASE_HEAD set — so the agent can resolve
// it the way a person would: fix the files, git add, git rebase --continue,
// push. AbortRebase restores the pre-rebase state if nobody does.
type RebaseConflictError struct {
	RepoPath string
	Branch   string
	Files    []string
	Err      error
}

func (e *RebaseConflictError) Error() string {
	return fmt.Sprintf("GIT_REBASE_CONFLICT: remote %s moved and rebasing onto it conflicts in %s", e.Branch, strings.Join(e.Files, ", "))
}

func (e *RebaseConflictError) Unwrap() error { return e.Err }

// Push pushes HEAD to its same-named branch on origin, retrying.
//
// A non-fast-forward rejection means the remote moved while this session was
// working — routine once several sessions run at once. It is recoverable:
// rebase onto the new remote head and push again, as many times as the
// retry budget allows, since with N writers the remote can move between the
// rebase and the push. A rebase that stops on conflicts returns a
// *RebaseConflictError with the rebase left in progress for the caller to
// resolve; other rebase failures abort the rebase and return
// GIT_PUSH_CONFLICT. Returning a rejection to the caller without this would
// lose work: the agent path deletes the session workspace afterwards
// (internal/execution/engine.go cleanup), taking unpushed commits with it.
func (o *Operations) Push(ctx context.Context, repoPath string) error {
	var lastErr error
	pushTarget := o.resolveRemote(ctx, repoPath)
	branch, _ := o.GetCurrentBranch(ctx, repoPath)
	if branch == "" || branch == "HEAD" {
		branch = "HEAD"
	}
	rejected := false

	for i := range o.PushRetries {
		if i > 0 && !rejected {
			time.Sleep(time.Duration(o.PushRetryDelaySeconds) * time.Second)
		}
		rejected = false

		err := o.runGitCommand(ctx, repoPath, "push", pushTarget, "HEAD")
		if err == nil {
			return nil
		}
		lastErr = err
		errStr := err.Error()

		switch {
		case strings.Contains(errStr, "non-fast-forward") || strings.Contains(errStr, "rejected"):
			rejected = true
			if rebaseErr := o.pullRebase(ctx, repoPath, pushTarget, branch); rebaseErr != nil {
				if files := o.ConflictedFiles(ctx, repoPath); len(files) > 0 {
					return &RebaseConflictError{RepoPath: repoPath, Branch: branch, Files: files, Err: rebaseErr}
				}
				// Leave the repo in a usable state rather than mid-rebase, so a
				// later retry or manual inspection isn't blocked by REBASE_HEAD.
				if abortErr := o.AbortRebase(ctx, repoPath); abortErr != nil {
					slog.Debug("git: rebase --abort after failed pull", "repo", repoPath, "error", abortErr)
				}
				return fmt.Errorf("GIT_PUSH_CONFLICT: %w (rebase onto remote failed: %v)", err, rebaseErr)
			}
			slog.Info("git: rebased onto moved remote, pushing again", "repo", repoPath, "branch", branch, "attempt", i+1)
			// Loop straight into the next push, no delay.
		case strings.Contains(errStr, "Authentication failed") || strings.Contains(errStr, "Permission denied"):
			return fmt.Errorf("GIT_AUTH_FAILURE: %w", err)
		}
	}

	if rejected {
		return fmt.Errorf("GIT_PUSH_CONFLICT: remote kept moving; push rejected after %d attempts: %w", o.PushRetries, lastErr)
	}
	return fmt.Errorf("GIT_NETWORK_ERROR: push failed after %d retries: %w", o.PushRetries, lastErr)
}

// PushToBranch pushes HEAD to a differently named branch on origin, for
// parking work that couldn't be merged onto its own branch.
func (o *Operations) PushToBranch(ctx context.Context, repoPath, branch string) error {
	pushTarget := o.resolveRemote(ctx, repoPath)
	if err := o.runGitCommand(ctx, repoPath, "push", pushTarget, "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("git push to %s failed: %w", branch, err)
	}
	return nil
}

// ConflictedFiles lists paths with unresolved merge conflicts.
func (o *Operations) ConflictedFiles(ctx context.Context, repoPath string) []string {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files
}

// RebaseInProgress reports whether repoPath is mid-rebase.
func (o *Operations) RebaseInProgress(ctx context.Context, repoPath string) bool {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", dir)
		cmd.Dir = repoPath
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		path := strings.TrimSpace(string(out))
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoPath, path)
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// ContinueRebase resumes a rebase whose conflicts have been staged,
// accepting the original commit messages.
func (o *Operations) ContinueRebase(ctx context.Context, repoPath string) error {
	return o.runGitCommandEnv(ctx, repoPath, []string{"GIT_EDITOR=true"}, "rebase", "--continue")
}

// RebaseHeadName returns the branch a rebase in progress is rewriting.
func (o *Operations) RebaseHeadName(ctx context.Context, repoPath string) (string, error) {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", dir+"/head-name")
		cmd.Dir = repoPath
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		path := strings.TrimSpace(string(out))
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoPath, path)
		}
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimPrefix(strings.TrimSpace(string(b)), "refs/heads/"), nil
		}
	}
	return "", fmt.Errorf("no rebase in progress")
}

// AbortRebase restores the branch to its pre-rebase state.
func (o *Operations) AbortRebase(ctx context.Context, repoPath string) error {
	return o.runGitCommand(ctx, repoPath, "rebase", "--abort")
}

// GetCurrentBranch returns the current branch name
func (o *Operations) GetCurrentBranch(ctx context.Context, repoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// RevertChanges discards all changes in the working tree (git checkout . && git clean -fd)
func (o *Operations) RevertChanges(ctx context.Context, repoPath string) error {
	if err := o.runGitCommand(ctx, repoPath, "checkout", "."); err != nil {
		return fmt.Errorf("git checkout failed: %w", err)
	}
	if err := o.runGitCommand(ctx, repoPath, "clean", "-fd"); err != nil {
		return fmt.Errorf("git clean failed: %w", err)
	}
	return nil
}

// PullRebase rebases the current branch onto its same-named branch on origin.
func (o *Operations) PullRebase(ctx context.Context, repoPath string) error {
	branch, _ := o.GetCurrentBranch(ctx, repoPath)
	if branch == "" || branch == "HEAD" {
		branch = "HEAD"
	}
	return o.pullRebase(ctx, repoPath, o.resolveRemote(ctx, repoPath), branch)
}

// pullRebase is PullRebase with the remote and branch already resolved.
// The branch is named explicitly: `pull origin HEAD` would rebase onto the
// remote's default branch, which is wrong for any other branch.
func (o *Operations) pullRebase(ctx context.Context, repoPath, remote, branch string) error {
	if err := o.runGitCommand(ctx, repoPath, "pull", "--rebase", remote, branch); err != nil {
		return fmt.Errorf("git pull --rebase failed: %w", err)
	}
	return nil
}

// ConfigureAuthor sets the git user.name and user.email for a repository
func (o *Operations) ConfigureAuthor(ctx context.Context, repoPath, author string) error {
	if err := o.runGitCommand(ctx, repoPath, "config", "user.name", author); err != nil {
		return fmt.Errorf("git config user.name failed: %w", err)
	}
	if err := o.runGitCommand(ctx, repoPath, "config", "user.email", author+"@bot.local"); err != nil {
		return fmt.Errorf("git config user.email failed: %w", err)
	}
	return nil
}

func (o *Operations) getDefaultBranch(ctx context.Context, repoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	// Output is like "refs/remotes/origin/main"
	parts := strings.Split(strings.TrimSpace(string(output)), "/")
	if len(parts) > 0 {
		return parts[len(parts)-1], nil
	}

	return "main", nil
}

func (o *Operations) runGitCommand(ctx context.Context, repoPath string, args ...string) error {
	return o.runGitCommandEnv(ctx, repoPath, nil, args...)
}

func (o *Operations) runGitCommandEnv(ctx context.Context, repoPath string, extraEnv []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return nil
}

// parseDiffStat parses git diff --stat output to get insertions and deletions
func parseDiffStat(output string) DiffSummary {
	summary := DiffSummary{}

	// Look for the summary line like "3 files changed, 120 insertions(+), 5 deletions(-)"
	re := regexp.MustCompile(`(\d+) insertions?\(\+\)`)
	if matches := re.FindStringSubmatch(output); len(matches) > 1 {
		summary.Insertions, _ = strconv.Atoi(matches[1])
	}

	re = regexp.MustCompile(`(\d+) deletions?\(-\)`)
	if matches := re.FindStringSubmatch(output); len(matches) > 1 {
		summary.Deletions, _ = strconv.Atoi(matches[1])
	}

	return summary
}
