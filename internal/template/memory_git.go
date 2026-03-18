package template

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// CommitAndPushMemory stages all changes in memoryDir, commits them, and
// pushes to origin if a remote is configured. No-op if memoryDir is not a
// git repository. Push failure is logged but not returned as an error.
func CommitAndPushMemory(memoryDir string) error {
	if memoryDir == "" {
		return nil
	}
	// Only proceed if memoryDir is a git repo
	if _, err := os.Stat(filepath.Join(memoryDir, ".git")); err != nil {
		return nil
	}

	// Stage all changes
	if err := gitRun(memoryDir, "add", "-A"); err != nil {
		return err
	}

	// Check if there's anything to commit
	out, err := gitOutput(memoryDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil // nothing to commit
	}

	// Commit
	msg := "[memory] " + time.Now().Format("2006-01-02")
	if err := gitRun(memoryDir, "commit", "-m", msg); err != nil {
		return err
	}

	// Push (best-effort — no remote is fine)
	if err := gitRun(memoryDir, "push"); err != nil {
		slog.Warn("memory git push failed (no remote configured?)", "error", err)
	}

	return nil
}

func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
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
