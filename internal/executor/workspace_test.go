package executor

import (
	"github.com/agent-runner/agent-runner/internal/rescue"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupWorkspace_RemovesDir(t *testing.T) {
	tmpRoot := t.TempDir()
	wsPath := filepath.Join(tmpRoot, "job-test")
	os.MkdirAll(wsPath, 0755)
	os.WriteFile(filepath.Join(wsPath, "file.txt"), []byte("data"), 0644)

	wm := NewWorkspaceManager(tmpRoot, 3600)
	err := wm.CleanupWorkspace(wsPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Error("workspace should have been removed")
	}
}

func TestCleanupWorkspace_EmptyPath(t *testing.T) {
	wm := NewWorkspaceManager("/tmp/test", 3600)
	err := wm.CleanupWorkspace("")
	if err != nil {
		t.Fatalf("expected nil error for empty path, got: %v", err)
	}
}

func TestCleanupWorkspace_Nonexistent(t *testing.T) {
	wm := NewWorkspaceManager("/tmp/test", 3600)
	err := wm.CleanupWorkspace("/nonexistent/path/workspace")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent path, got: %v", err)
	}
}

func TestCleanupStaleWorkspaces_RemovesOld(t *testing.T) {
	tmpRoot := t.TempDir()

	// Create an "old" workspace directory
	oldDir := filepath.Join(tmpRoot, "job-old")
	os.MkdirAll(oldDir, 0755)
	// Set mod time to 2 hours ago
	oldTime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(oldDir, oldTime, oldTime)

	// Create a "fresh" workspace directory
	freshDir := filepath.Join(tmpRoot, "job-fresh")
	os.MkdirAll(freshDir, 0755)

	// MaxRuntimeSeconds = 3600 (1 hour), so the 2-hour-old dir should be cleaned
	wm := NewWorkspaceManager(tmpRoot, 3600)
	err := wm.CleanupStaleWorkspaces()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Old workspace should be removed
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Error("old workspace should have been removed")
	}

	// Fresh workspace should still exist
	if _, err := os.Stat(freshDir); os.IsNotExist(err) {
		t.Error("fresh workspace should not have been removed")
	}
}

func TestCleanupStaleWorkspaces_NonexistentTmpRoot(t *testing.T) {
	wm := NewWorkspaceManager("/nonexistent/tmp/root", 3600)
	err := wm.CleanupStaleWorkspaces()
	if err != nil {
		t.Fatalf("expected nil error for nonexistent tmp root, got: %v", err)
	}
}

func TestCleanupStaleWorkspaces_SkipsFiles(t *testing.T) {
	tmpRoot := t.TempDir()

	// Create a regular file (not a directory) — should be skipped
	os.WriteFile(filepath.Join(tmpRoot, "not-a-dir"), []byte("data"), 0644)

	wm := NewWorkspaceManager(tmpRoot, 3600)
	err := wm.CleanupStaleWorkspaces()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// File should still exist (not cleaned up)
	if _, err := os.Stat(filepath.Join(tmpRoot, "not-a-dir")); os.IsNotExist(err) {
		t.Error("regular file should not be cleaned up")
	}
}

// [C1] PrepareAgentWorkspace must clean up its session dir when setup fails.
func TestPrepareAgentWorkspace_CleansUpOnError(t *testing.T) {
	tmpRoot := t.TempDir()
	wm := NewWorkspaceManager(tmpRoot, 3600)

	// Pre-create the workspace/ subdir so setup gets past the first MkdirAll,
	// then plant a regular *file* at the state/ path so MkdirAll(stateDir) fails.
	sessionID := "c1test"
	workspacePath := filepath.Join(tmpRoot, "session-"+sessionID)
	if err := os.MkdirAll(filepath.Join(workspacePath, "workspace"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "state"), []byte("block"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := wm.PrepareAgentWorkspace(sessionID, "")
	if err == nil {
		t.Fatal("expected error when stateDir cannot be created")
	}

	if _, statErr := os.Stat(workspacePath); !os.IsNotExist(statErr) {
		t.Errorf("expected workspace dir to be removed after setup failure, but it still exists at %s", workspacePath)
	}
}

// [H4] copyDir must skip both directory and file symlinks.
func TestCopyDir_SkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Regular file — should be copied.
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Directory symlink — should be skipped.
	realTarget := t.TempDir()
	if err := os.WriteFile(filepath.Join(realTarget, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTarget, filepath.Join(src, "dirlink")); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	// File symlink — should also be skipped.
	if err := os.Symlink(filepath.Join(src, "real.txt"), filepath.Join(src, "filelink.txt")); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "real.txt")); err != nil {
		t.Error("regular file should be present in dst")
	}
	if _, err := os.Lstat(filepath.Join(dst, "dirlink")); !os.IsNotExist(err) {
		t.Error("directory symlink must be skipped — not present in dst")
	}
	if _, err := os.Lstat(filepath.Join(dst, "filelink.txt")); !os.IsNotExist(err) {
		t.Error("file symlink must be skipped — not present in dst")
	}
	// Secret file must not be reachable via the skipped dirlink.
	if _, err := os.Stat(filepath.Join(dst, "dirlink", "secret.txt")); !os.IsNotExist(err) {
		t.Error("secret.txt must not be reachable via skipped dirlink")
	}
}

// [H4] copyFile must resolve a file symlink to its content instead of
// reproducing the link, preventing dangerous symlinks from escaping workspace.
func TestCopyFile_ResolvesSymlinkContent(t *testing.T) {
	dir := t.TempDir()

	realPath := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(realPath, []byte("real content"), 0644); err != nil {
		t.Fatal(err)
	}

	linkPath := filepath.Join(dir, "link.txt")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	dstPath := filepath.Join(dir, "dst.txt")
	if err := copyFile(linkPath, dstPath); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	// dst must be a regular file, not a symlink.
	info, err := os.Lstat(dstPath)
	if err != nil {
		t.Fatalf("os.Lstat(dst): %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("dst should be a regular file, not a symlink")
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("ReadFile(dst): %v", err)
	}
	if string(data) != "real content" {
		t.Errorf("expected %q, got %q", "real content", string(data))
	}
}

// Removing a workspace keeps its unsaved work first and says so; a clean
// workspace is just removed.
func TestCleanupWorkspace_RescuesUnsavedWork(t *testing.T) {
	root := t.TempDir()
	w := &WorkspaceManager{TmpRoot: root, RescueRoot: filepath.Join(root, "rescued")}
	var told []string
	w.OnRescue = func(path string, rep rescue.Report) { told = append(told, rep.Summary()) }

	ws := filepath.Join(root, "session-1")
	os.MkdirAll(filepath.Join(ws, "workspace", "notes"), 0o755)
	os.WriteFile(filepath.Join(ws, "workspace", "notes", "plan.md"), []byte("# M1"), 0o644)
	if err := w.CleanupWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("the workspace was not removed")
	}
	kept, _ := filepath.Glob(filepath.Join(root, "rescued", "session-1-*", "files", "notes", "plan.md"))
	if len(kept) != 1 || len(told) != 1 {
		t.Fatalf("kept %v, told %v", kept, told)
	}

	clean := filepath.Join(root, "session-2")
	os.MkdirAll(filepath.Join(clean, "workspace"), 0o755)
	if err := w.CleanupWorkspace(clean); err != nil || len(told) != 1 {
		t.Fatalf("a clean workspace: err %v, told %v", err, told)
	}
}
