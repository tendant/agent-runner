package template

import (
	"bytes"
	"fmt"
	"log/slog"
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
	Pushed        bool   // a push was attempted
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

	// Push
	pushTarget := injectToken(remote, creds.Token)
	env := GitSSHEnv(remote, creds.SSHKey)
	if err := gitRunEnv(memoryDir, env, "push", "-u", "origin", pushTarget); err != nil {
		slog.Warn("memory git push failed", "error", err)
	} else {
		res.Pushed = true
	}

	return res, nil
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
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}

	msg := "[memory] " + time.Now().Format("2006-01-02")
	if err := gitRunEnv(memoryDir, nil, "commit", "-m", msg); err != nil {
		return err
	}

	// Resolve push target and SSH env from stored remote
	remote := ""
	if remoteOut, err := gitOutput(memoryDir, "remote", "get-url", "origin"); err == nil {
		remote = strings.TrimSpace(string(remoteOut))
	}
	pushTarget := injectToken(remote, creds.Token)
	env := GitSSHEnv(remote, creds.SSHKey)

	if err := gitRunEnv(memoryDir, env, "push", "-u", "origin", pushTarget); err != nil {
		slog.Warn("memory git push failed (no remote configured?)", "error", err)
	}

	return nil
}

// injectToken rewrites an HTTPS remote URL to embed the token as credentials.
// The original remote stored in .git/config is never modified.
// Returns remote unchanged if it is not an HTTPS URL or token is empty.
func injectToken(remote, token string) string {
	if token == "" || remote == "" {
		return remote
	}
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(remote, prefix) {
			return prefix + "oauth2:" + token + "@" + remote[len(prefix):]
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
