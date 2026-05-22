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
	User   string // MEMORY_GIT_USER  — git username; defaults to "oauth2" when empty
	SSHKey string // MEMORY_GIT_SSH_KEY — path to private key; sets GIT_SSH_COMMAND
}

// InitMemoryGitResult describes what InitMemoryGit did.
type InitMemoryGitResult struct {
	Initialised   bool   // git init was run (new repo)
	RemoteOld     string // previous remote URL, empty if none existed
	RemoteNew     string // remote URL after the call
	RemoteChanged bool   // remote was added or updated
}

// configureCredHelper writes a git credential.helper to the repo's local
// config. The helper echoes username and reads MEMORY_GIT_TOKEN from the
// process environment at runtime, so the token itself is never stored.
// user is baked into the script; re-call when MEMORY_GIT_USER changes.
func configureCredHelper(memoryDir, user string) error {
	if user == "" {
		user = "oauth2"
	}
	helper := fmt.Sprintf(`!f() { echo username=%s; echo "password=$MEMORY_GIT_TOKEN"; }; f`, user)
	return gitRunEnv(memoryDir, nil, "config", "--local", "credential.helper", helper)
}

// InitMemoryGit initialises memoryDir as a git repository backed by remote.
// Idempotent: safe to call on an already-initialised repo. If the remote
// differs from the existing one it is updated via set-url. Credentials are
// applied via a git credential helper — the stored remote URL stays clean.
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

	// Configure credential helper when a token is available so subsequent
	// push/pull operations authenticate without embedding the token in the URL.
	if creds.Token != "" {
		if err := configureCredHelper(memoryDir, creds.User); err != nil {
			slog.Warn("memory git: failed to configure credential helper", "error", err)
		}
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

	env := GitSSHEnv(remote, creds.SSHKey)

	// Ensure credential helper is current (user may have changed since init).
	if creds.Token != "" {
		if err := configureCredHelper(memoryDir, creds.User); err != nil {
			slog.Warn("memory git: failed to configure credential helper", "error", err)
		}
	}

	if err := gitRunEnv(memoryDir, env, "pull", "--rebase", "origin", "HEAD"); err != nil {
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
		// Ensure a local identity exists — avoids commit failure in environments
		// (e.g. Docker) where no global git user is configured.
		_ = gitRunEnv(memoryDir, nil, "config", "user.email", "agent-runner@local")
		_ = gitRunEnv(memoryDir, nil, "config", "user.name", "agent-runner")
		if err := gitRunEnv(memoryDir, nil, "commit", "-m", msg); err != nil {
			return err
		}
	}

	// Push only when a remote is configured.
	remote := ""
	if remoteOut, err := gitOutput(memoryDir, "remote", "get-url", "origin"); err == nil {
		remote = strings.TrimSpace(string(remoteOut))
	}
	if remote == "" {
		return nil
	}

	// Ensure credential helper is current before pushing.
	if creds.Token != "" {
		if err := configureCredHelper(memoryDir, creds.User); err != nil {
			slog.Warn("memory git: failed to configure credential helper", "error", err)
		}
	}

	env := GitSSHEnv(remote, creds.SSHKey)

	if err := pushMemory(memoryDir, env, remote, creds.Token); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	return nil
}

// pushMemory pushes local commits to origin. If the remote repository does not
// exist and a token is available, it attempts to create it via the Gitea API
// and retries. If the push is rejected because the remote is ahead
// (non-fast-forward), it pulls with rebase and retries once.
// Authentication is handled by the credential helper configured in the repo.
func pushMemory(memoryDir string, env []string, remote, token string) error {
	doPush := func() error {
		return gitRunEnv(memoryDir, env, "push", "origin", "HEAD")
	}

	pushErr := doPush()
	if pushErr == nil {
		return nil
	}

	// Whenever push fails and we have a token, attempt to create the repo via
	// the Gitea API — regardless of what the push error says, because the server
	// may return a permission/auth error before it even checks whether the repo
	// exists. Only retry the push if creation actually succeeded (HTTP 201).
	if token != "" {
		if createErr := tryCreateGiteaRepo(remote, token); createErr == nil {
			return doPush()
		}
	}

	// Remote is ahead — sync then retry. Try rebase first for a clean history;
	// fall back to a merge (with --allow-unrelated-histories) when the local
	// and remote histories have diverged completely (e.g. fresh local init vs
	// existing remote). Always prefer local content on conflicts (-Xours).
	rebaseErr := gitRunEnv(memoryDir, env, "pull", "--rebase", "-Xtheirs", "origin", "HEAD")
	if rebaseErr != nil {
		// [H6] If abort itself fails the repo is stuck in an in-progress rebase;
		// the merge fallback would immediately fail with "unfinished rebase".
		// Surface both errors so the operator can run 'git rebase --abort' manually.
		if abortErr := gitRunEnv(memoryDir, nil, "rebase", "--abort"); abortErr != nil {
			return fmt.Errorf("pull --rebase failed (%w); rebase --abort also failed (%v) — run 'git rebase --abort' in %s manually", rebaseErr, abortErr, memoryDir)
		}
		// Rebase aborted cleanly. Fall back to a merge that allows unrelated
		// histories, keeping local content on conflict.
		mergeErr := gitRunEnv(memoryDir, env, "pull", "--no-rebase",
			"--allow-unrelated-histories", "-Xours", "origin", "HEAD")
		if mergeErr != nil {
			if isRepoNotFound(mergeErr) && token == "" {
				return fmt.Errorf("remote repository not found — set MEMORY_GIT_TOKEN to enable auto-create, or create the repo manually: %w", mergeErr)
			}
			return fmt.Errorf("sync with remote failed: %w", mergeErr)
		}
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
// REST API. It parses the remote URL for host, owner, and repo name.
// It tries the org endpoint first. If the owner is not an org (or the token
// lacks org-write permission), it falls back to the user endpoint — but only
// when the authenticated user's login matches the URL owner, so the repo is
// never created under the wrong account.
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

	doPost := func(endpoint string) bool {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "token "+token)
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusCreated
	}

	// Try org endpoint first.
	if doPost(apiBase + "/orgs/" + owner + "/repos") {
		slog.Info("memory git: created remote repo via org API", "owner", owner, "repo", repoName)
		return nil
	}

	// Fall back to user endpoint only when the URL owner matches the
	// authenticated user — avoids creating under the wrong account.
	if login := giteaUserLogin(apiBase, token, client); login == owner {
		if doPost(apiBase + "/user/repos") {
			slog.Info("memory git: created remote repo via user API", "owner", owner, "repo", repoName)
			return nil
		}
	}

	return fmt.Errorf("could not create %s/%s via Gitea API", owner, repoName)
}

// giteaUserLogin returns the login name of the authenticated Gitea user,
// or "" if the request fails.
func giteaUserLogin(apiBase, token string, client *http.Client) string {
	req, err := http.NewRequest(http.MethodGet, apiBase+"/user", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return ""
	}
	return u.Login
}

// InjectToken rewrites an HTTPS remote URL to embed the token as credentials.
// user is the git username (e.g. the Gitea account name); when empty it
// defaults to "oauth2" which Gitea accepts for personal access tokens.
// The original remote stored in .git/config is never modified.
// Returns remote unchanged if it is not an HTTPS URL, token is empty, or the
// URL already contains credentials (user:pass@ prefix) — double-injecting
// produces a mangled URL that git rejects.
func InjectToken(remote, token, user string) string {
	if token == "" || remote == "" {
		return remote
	}
	gitUser := user
	if gitUser == "" {
		gitUser = "oauth2"
	}
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(remote, prefix) {
			rest := remote[len(prefix):]
			if strings.Contains(rest, "@") {
				// Credentials already embedded in the URL — leave it alone.
				return remote
			}
			return prefix + gitUser + ":" + token + "@" + rest
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
