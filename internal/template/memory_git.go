package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MemoryGitCreds holds optional credentials for memory git operations.
type MemoryGitCreds struct {
	Token  string // MEMORY_GIT_TOKEN — injected into HTTPS push URLs at runtime
	SSHKey string // MEMORY_GIT_SSH_KEY — path to private key; sets GIT_SSH_COMMAND
}

// InitMemoryGitResult describes what InitMemoryGit did.
type InitMemoryGitResult struct {
	Initialised   bool   // git init was run (new repo)
	RemoteOld     string // previous remote URL, empty if none existed
	RemoteNew     string // remote URL after the call
	RemoteChanged bool   // remote was added or updated
}

// InitMemoryGit initialises memoryDir as a git repository backed by remote.
// Idempotent: safe to call on an already-initialised repo. If the remote
// differs from the existing one it is updated via set-url. Credentials are
// applied at push time only — the stored remote URL stays clean.
func InitMemoryGit(memoryDir, remote string, creds MemoryGitCreds) (InitMemoryGitResult, error) {
	var res InitMemoryGitResult
	if memoryDir == "" {
		return res, fmt.Errorf("memoryDir is required")
	}
	if remote == "" {
		return res, fmt.Errorf("remote URL is required")
	}

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return res, fmt.Errorf("create memory dir: %w", err)
	}

	// git init (idempotent)
	if _, err := os.Stat(filepath.Join(memoryDir, ".git")); os.IsNotExist(err) {
		if err := gitRunEnv(memoryDir, nil, "init"); err != nil {
			return res, fmt.Errorf("git init: %w", err)
		}
		res.Initialised = true
	}

	// Check existing remote
	existing := ""
	if out, err := gitOutput(memoryDir, "remote", "get-url", "origin"); err == nil {
		existing = strings.TrimSpace(string(out))
	}
	res.RemoteOld = existing
	res.RemoteNew = remote

	switch {
	case existing == "":
		if err := gitRunEnv(memoryDir, nil, "remote", "add", "origin", remote); err != nil {
			return res, fmt.Errorf("git remote add: %w", err)
		}
		res.RemoteChanged = true
	case existing != remote:
		if err := gitRunEnv(memoryDir, nil, "remote", "set-url", "origin", remote); err != nil {
			return res, fmt.Errorf("git remote set-url: %w", err)
		}
		res.RemoteChanged = true
	default:
		// Same remote — still attempt push in case there are uncommitted files
	}

	// Stage and commit any existing content
	if err := gitRunEnv(memoryDir, nil, "add", "-A"); err != nil {
		return res, fmt.Errorf("git add: %w", err)
	}
	status, err := gitOutput(memoryDir, "status", "--porcelain")
	if err != nil {
		return res, err
	}
	if len(bytes.TrimSpace(status)) > 0 {
		msg := "[memory] init " + time.Now().Format("2006-01-02")
		// Set a local identity if none is configured globally (avoids commit failure)
		_ = gitRunEnv(memoryDir, nil, "config", "user.email", "agent-runner@local")
		_ = gitRunEnv(memoryDir, nil, "config", "user.name", "agent-runner")
		if err := gitRunEnv(memoryDir, nil, "commit", "-m", msg); err != nil {
			return res, fmt.Errorf("git commit: %w", err)
		}
	}

	return res, nil
}

// PullMemory fetches the latest commits from origin and rebases local commits
// on top. Uses --rebase so local work is preserved when the remote is ahead.
// Returns the remote URL on success.
func PullMemory(memoryDir string, creds MemoryGitCreds) (string, error) {
	if memoryDir == "" {
		return "", fmt.Errorf("memoryDir is required")
	}
	if _, err := os.Stat(filepath.Join(memoryDir, ".git")); err != nil {
		return "", fmt.Errorf("memory dir is not a git repo — run /memory git <remote-url> first")
	}

	remote := ""
	if out, err := gitOutput(memoryDir, "remote", "get-url", "origin"); err == nil {
		remote = strings.TrimSpace(string(out))
	}
	if remote == "" {
		return "", fmt.Errorf("no remote configured — run /memory git <remote-url> first")
	}

	pullTarget := InjectToken(remote, creds.Token)
	env := GitSSHEnv(remote, creds.SSHKey)

	if err := gitRunEnv(memoryDir, env, "pull", "--rebase", pullTarget, "HEAD"); err != nil {
		_ = gitRunEnv(memoryDir, nil, "rebase", "--abort")
		return "", fmt.Errorf("pull --rebase failed, rebase aborted: %w", err)
	}

	return remote, nil
}

// CommitAndPushMemory stages all changes in memoryDir, commits them, and
// pushes to origin. No-op if memoryDir is not a git repository.
// Push failure is logged but not returned as an error.
func CommitAndPushMemory(memoryDir string, creds MemoryGitCreds) error {
	if memoryDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(memoryDir, ".git")); err != nil {
		return nil
	}

	if err := gitRunEnv(memoryDir, nil, "add", "-A"); err != nil {
		return err
	}

	out, err := gitOutput(memoryDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(out)) > 0 {
		msg := "[memory] " + time.Now().Format("2006-01-02")
		if err := gitRunEnv(memoryDir, nil, "commit", "-m", msg); err != nil {
			return err
		}
	}

	// Always push — even if nothing new was committed, there may be local
	// commits that haven't been pushed yet.
	remote := ""
	if remoteOut, err := gitOutput(memoryDir, "remote", "get-url", "origin"); err == nil {
		remote = strings.TrimSpace(string(remoteOut))
	}
	pushTarget := InjectToken(remote, creds.Token)
	env := GitSSHEnv(remote, creds.SSHKey)

	if err := pushMemory(memoryDir, env, remote, pushTarget, creds.Token); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	return nil
}

// pushMemory pushes local commits to the remote. If the remote repository does
// not exist and a token is available, it attempts to create it via the Gitea
// API and retries. If the push is rejected because the remote is ahead
// (non-fast-forward), it pulls with rebase and retries once.
//
// When a token was injected into pushTarget, we push directly to the
// credentialed URL so git treats it as a repository rather than a refspec
// (passing a URL after a remote name causes git to interpret it as src:dst).
func pushMemory(memoryDir string, env []string, remote, pushTarget, token string) error {
	doPush := func() error {
		if pushTarget != remote {
			return gitRunEnv(memoryDir, env, "push", pushTarget, "HEAD")
		}
		return gitRunEnv(memoryDir, env, "push", "origin", "HEAD")
	}

	pushErr := doPush()
	if pushErr == nil {
		return nil
	}

	// If the remote repo doesn't exist, try to create it via API then retry.
	if isRepoNotFound(pushErr) {
		if createErr := tryCreateGiteaRepo(remote, token); createErr == nil {
			return doPush()
		}
		return pushErr
	}

	// Push rejected (non-fast-forward) — pull with rebase then retry.
	pullTarget := pushTarget
	if err := gitRunEnv(memoryDir, env, "pull", "--rebase", pullTarget, "HEAD"); err != nil {
		_ = gitRunEnv(memoryDir, nil, "rebase", "--abort")
		return fmt.Errorf("pull --rebase failed, rebase aborted: %w", err)
	}

	return doPush()
}

// isRepoNotFound reports whether a git error looks like the remote repository
// does not exist (as opposed to auth failure, network error, etc.).
func isRepoNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist")
}

// tryCreateGiteaRepo creates a repository on a Gitea/Forgejo host via its
// REST API. It parses the remote URL for host, owner, and repo name, then
// tries the org endpoint first and the user endpoint as a fallback.
// Returns nil if the repo was created; non-nil if creation was not possible.
func tryCreateGiteaRepo(remote, token string) error {
	if token == "" {
		return fmt.Errorf("no token")
	}
	var scheme, rest string
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(remote, prefix) {
			scheme = strings.TrimSuffix(prefix, "://")
			rest = remote[len(prefix):]
			break
		}
	}
	if scheme == "" {
		return fmt.Errorf("not an HTTPS remote")
	}
	// Strip embedded credentials (oauth2:token@host/…)
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 {
		return fmt.Errorf("cannot parse owner/repo from %s", remote)
	}
	host := parts[0]
	owner := parts[1]
	repoName := strings.TrimSuffix(parts[2], ".git")

	nameJSON, _ := json.Marshal(repoName)
	body := fmt.Sprintf(`{"name":%s,"private":true,"auto_init":false}`, string(nameJSON))
	apiBase := scheme + "://" + host + "/api/v1"
	client := &http.Client{Timeout: 15 * time.Second}

	for _, endpoint := range []string{
		apiBase + "/orgs/" + owner + "/repos",
		apiBase + "/user/repos",
	} {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "token "+token)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			slog.Info("memory git: created remote repo via API", "owner", owner, "repo", repoName)
			return nil
		}
	}
	return fmt.Errorf("could not create %s/%s via Gitea API", owner, repoName)
}

// InjectToken rewrites an HTTPS remote URL to embed the token as credentials.
// The original remote stored in .git/config is never modified.
// Returns remote unchanged if it is not an HTTPS URL, token is empty, or the
// URL already contains credentials (user:pass@ prefix) — double-injecting
// produces a mangled URL that git rejects.
func InjectToken(remote, token string) string {
	if token == "" || remote == "" {
		return remote
	}
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(remote, prefix) {
			rest := remote[len(prefix):]
			if strings.Contains(rest, "@") {
				// Credentials already embedded in the URL — leave it alone.
				return remote
			}
			return prefix + "oauth2:" + token + "@" + rest
		}
	}
	return remote
}

// GitSSHEnv returns env vars to use a specific SSH key for git operations.
// Returns nil when SSHKey is empty or the remote is not SSH-based.
func GitSSHEnv(remote, sshKey string) []string {
	if sshKey == "" {
		return nil
	}
	// Only apply for SSH remotes
	if strings.HasPrefix(remote, "https://") || strings.HasPrefix(remote, "http://") {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=ssh -i " + sshKey + " -o StrictHostKeyChecking=no"}
}

func gitRunEnv(dir string, env []string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &gitError{args: args, err: err, stderr: stderr.String()}
	}
	return nil
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Output()
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	if e.stderr != "" {
		return e.stderr
	}
	return e.err.Error()
}
