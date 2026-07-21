package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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

// Push pushes to origin with retry logic
func (o *Operations) Push(ctx context.Context, repoPath string) error {
	var lastErr error
	pushTarget := o.resolveRemote(ctx, repoPath)

	for i := range o.PushRetries {
		if i > 0 {
			time.Sleep(time.Duration(o.PushRetryDelaySeconds) * time.Second)
		}

		err := o.runGitCommand(ctx, repoPath, "push", pushTarget, "HEAD")
		if err == nil {
			return nil
		}

		lastErr = err
		errStr := err.Error()

		// Check for non-retryable errors
		if strings.Contains(errStr, "non-fast-forward") ||
			strings.Contains(errStr, "rejected") {
			return fmt.Errorf("GIT_PUSH_CONFLICT: %w", err)
		}
		if strings.Contains(errStr, "Authentication failed") ||
			strings.Contains(errStr, "Permission denied") {
			return fmt.Errorf("GIT_AUTH_FAILURE: %w", err)
		}
	}

	return fmt.Errorf("GIT_NETWORK_ERROR: push failed after %d retries: %w", o.PushRetries, lastErr)
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

// PullRebase pulls from origin with rebase strategy
func (o *Operations) PullRebase(ctx context.Context, repoPath string) error {
	if err := o.runGitCommand(ctx, repoPath, "pull", "--rebase", "origin", "HEAD"); err != nil {
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
