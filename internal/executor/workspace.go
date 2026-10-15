package executor

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorkspaceManager handles workspace creation and cleanup
type WorkspaceManager struct {
	TmpRoot           string
	MaxRuntimeSeconds int
}

// NewWorkspaceManager creates a new workspace manager
func NewWorkspaceManager(tmpRoot string, maxRuntimeSeconds int) *WorkspaceManager {
	return &WorkspaceManager{
		TmpRoot:           tmpRoot,
		MaxRuntimeSeconds: maxRuntimeSeconds,
	}
}

// JobWorkspacePath is where a one-shot job's checkout goes.
func (w *WorkspaceManager) JobWorkspacePath(jobID string) string {
	return filepath.Join(w.TmpRoot, "job-"+jobID)
}

// CleanupWorkspace removes a workspace directory
func (w *WorkspaceManager) CleanupWorkspace(workspacePath string) error {
	if workspacePath == "" {
		return nil
	}
	return os.RemoveAll(workspacePath)
}

// CleanupStaleWorkspaces removes workspaces older than max runtime
func (w *WorkspaceManager) CleanupStaleWorkspaces() error {
	entries, err := os.ReadDir(w.TmpRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // tmp directory doesn't exist yet
		}
		return fmt.Errorf("failed to read tmp directory: %w", err)
	}

	cutoff := time.Now().Add(-time.Duration(w.MaxRuntimeSeconds) * time.Second)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Only clean workspace directories; leave other subdirs (e.g. conversations/) alone.
		name := entry.Name()
		if !strings.HasPrefix(name, "session-") && !strings.HasPrefix(name, "job-") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Check if directory is older than cutoff
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(w.TmpRoot, entry.Name())
			if err := os.RemoveAll(path); err != nil {
				slog.Warn("failed to clean up stale workspace", "path", path, "error", err)
			}
		}
	}

	return nil
}

// PrepareAgentWorkspace creates a workspace with workspace/ and state/ subdirectories.
// workspace/ is the agent's CWD — repos, _send/, _progress.json live here.
// state/ is runner-managed bookkeeping (TODO.md), invisible to the agent.
// If skillsDir is set, skills are copied to .claude/skills/ and .agents/skills/ so
// Claude Code, opencode, and Codex all discover them. Repos are checked out
// into workspace/ by the caller (internal/repos).
func (w *WorkspaceManager) PrepareAgentWorkspace(sessionID, skillsDir string) (string, error) {
	return w.PrepareAgentWorkspaceAt(filepath.Join(w.TmpRoot, "session-"+sessionID), skillsDir)
}

// TaskWorkspacePath is where a multi-turn task keeps its workspace between
// turns. The "task-" prefix keeps it out of CleanupStaleWorkspaces, which
// only sweeps session-* and job-* directories.
func (w *WorkspaceManager) TaskWorkspacePath(taskKey string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, taskKey)
	return filepath.Join(w.TmpRoot, "task-"+safe)
}

// PrepareAgentWorkspaceAt builds an agent workspace rooted at workspacePath
// (see PrepareAgentWorkspace for the layout).
func (w *WorkspaceManager) PrepareAgentWorkspaceAt(workspacePath, skillsDir string) (string, error) {

	// [C1] Clean up the session directory if setup fails partway through, so
	// partially-created directories don't accumulate in tmp/.
	var setupDone bool
	defer func() {
		if !setupDone {
			if err := os.RemoveAll(workspacePath); err != nil {
				slog.Warn("workspace: failed to clean up partial setup", "path", workspacePath, "error", err)
			}
		}
	}()

	if err := os.MkdirAll(w.TmpRoot, 0755); err != nil {
		return "", fmt.Errorf("failed to create tmp directory: %w", err)
	}

	agentDir := filepath.Join(workspacePath, "workspace")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create workspace directory: %w", err)
	}

	stateDir := filepath.Join(workspacePath, "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create state directory: %w", err)
	}

	// Pre-populate skills into both .claude/skills/ (Claude Code + opencode) and
	// .agents/skills/ (Codex + opencode) so all supported CLIs discover them.
	if skillsDir != "" {
		for _, dst := range []string{
			filepath.Join(agentDir, ".claude", "skills"),
			filepath.Join(agentDir, ".agents", "skills"),
		} {
			if err := copyDir(skillsDir, dst); err != nil {
				slog.Warn("workspace: failed to copy skills", "dst", dst, "error", err)
			}
		}
	}

	setupDone = true
	return workspacePath, nil
}

// copyDir recursively copies a directory. Symlinks are resolved to their
// content rather than reproduced, preventing agents from placing links that
// point outside the workspace into persistent storage (memory dir, outputs).
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		// [H4] Skip symlinks at the directory level — copyFile handles them at
		// the file level, but a symlink to a directory would recurse outside
		// the workspace. Skipping is safer than attempting to resolve.
		if entry.Type()&os.ModeSymlink != 0 {
			slog.Warn("workspace: skipping symlink", "path", filepath.Join(src, entry.Name()))
			continue
		}

		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyFile copies a single file. Symlinks are resolved to their content
// rather than reproduced. [H4]
func copyFile(src, dst string) error {
	// Use Lstat so we can detect symlinks before os.ReadFile follows them.
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}

	// [H4] Resolve symlinks to content instead of reproducing the link.
	// Re-creating a symlink whose target points outside the workspace would
	// allow content from arbitrary host paths to land in the memory dir or outputs.
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		data, err := os.ReadFile(src) // follows the symlink to read content
		if err != nil {
			slog.Warn("workspace: skipping unreadable symlink", "src", src, "error", err)
			return nil
		}
		return os.WriteFile(dst, data, 0644)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, srcInfo.Mode())
}
