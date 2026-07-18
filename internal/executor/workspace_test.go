package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareWorkspace_CreatesAndCopies(t *testing.T) {
	tmpRoot := t.TempDir()
	projectDir := t.TempDir()

	// Create some project files
	os.WriteFile(filepath.Join(projectDir, "main.go"), []byte("package main"), 0644)
	os.MkdirAll(filepath.Join(projectDir, "pkg"), 0755)
	os.WriteFile(filepath.Join(projectDir, "pkg", "lib.go"), []byte("package pkg"), 0644)

	wm := NewWorkspaceManager(tmpRoot, 3600)
	wsPath, err := wm.PrepareWorkspace(projectDir, "test-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify workspace was created
	if _, err := os.Stat(wsPath); os.IsNotExist(err) {
		t.Fatal("workspace directory was not created")
	}

	// Verify files were copied
	data, err := os.ReadFile(filepath.Join(wsPath, "main.go"))
	if err != nil {
		t.Fatalf("main.go not copied: %v", err)
	}
	if string(data) != "package main" {
		t.Errorf("main.go content mismatch: %q", string(data))
	}

	data, err = os.ReadFile(filepath.Join(wsPath, "pkg", "lib.go"))
	if err != nil {
		t.Fatalf("pkg/lib.go not copied: %v", err)
	}
	if string(data) != "package pkg" {
		t.Errorf("pkg/lib.go content mismatch: %q", string(data))
	}
}

func TestPrepareWorkspace_JobIDInPath(t *testing.T) {
	tmpRoot := t.TempDir()
	projectDir := t.TempDir()

	wm := NewWorkspaceManager(tmpRoot, 3600)
	wsPath, err := wm.PrepareWorkspace(projectDir, "abc-456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := filepath.Join(tmpRoot, "job-abc-456")
	if wsPath != expected {
		t.Errorf("expected path %q, got %q", expected, wsPath)
	}
}

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

	_, _, err := wm.PrepareAgentWorkspace("", sessionID, nil, "", "", "", "")
	if err == nil {
		t.Fatal("expected error when stateDir cannot be created")
	}

	if _, statErr := os.Stat(workspacePath); !os.IsNotExist(statErr) {
		t.Errorf("expected workspace dir to be removed after setup failure, but it still exists at %s", workspacePath)
	}
}

// [M7] PrepareAgentWorkspace must return repos that are listed in sharedRepos
// but absent from the cache so the caller can surface them as warnings.
func TestPrepareAgentWorkspace_ReturnsMissingRepos(t *testing.T) {
	tmpRoot := t.TempDir()
	repoCacheRoot := t.TempDir()
	wm := NewWorkspaceManager(tmpRoot, 3600)

	// "present" exists in cache; "absent" does not.
	if err := os.MkdirAll(filepath.Join(repoCacheRoot, "present"), 0755); err != nil {
		t.Fatal(err)
	}

	_, missing, err := wm.PrepareAgentWorkspace(repoCacheRoot, "m7test", []string{"present", "absent"}, "", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 1 || missing[0] != "absent" {
		t.Errorf(`expected missing=["absent"], got %v`, missing)
	}
}

func TestPrepareAgentWorkspace_EmptyMissingWhenAllPresent(t *testing.T) {
	tmpRoot := t.TempDir()
	repoCacheRoot := t.TempDir()
	wm := NewWorkspaceManager(tmpRoot, 3600)

	if err := os.MkdirAll(filepath.Join(repoCacheRoot, "myrepo"), 0755); err != nil {
		t.Fatal(err)
	}

	_, missing, err := wm.PrepareAgentWorkspace(repoCacheRoot, "m7ok", []string{"myrepo"}, "", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("expected no missing repos, got %v", missing)
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

// ConfigureCredHelper must win over an inherited credential.helper from a
// config level outside the repo's own --local config (e.g. a system-level
// macOS osxkeychain entry with a stale credential for the same host cached
// by an unrelated project). A naive `git config credential.helper <value>`
// only appends to the local file — it does not suppress that inherited
// entry, and git's credential resolution stops at the first helper that
// returns a full username+password, so the stale one silently wins.
func TestConfigureCredHelper_OverridesInheritedHelper(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()
	if err := exec.Command("git", "-C", repoDir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Simulate a system/global-level credential.helper with a decoy
	// credential for the same host, via GIT_CONFIG_GLOBAL (redirects where
	// git reads "global" config from — read before local, exactly like a
	// real system-level entry would be).
	fakeGlobalConfig := filepath.Join(t.TempDir(), "gitconfig-global")
	// Write via `git config -f` (not a raw ini string) so the shell
	// expression's semicolons and braces are escaped/quoted exactly as git
	// itself would — a hand-written unquoted value would have its `;`
	// parsed as a config-file comment, truncating it into a broken (and
	// therefore non-competing) helper, which would make this test pass
	// vacuously regardless of whether the fix works.
	decoyHelper := `!f() { echo username=decoy-user; echo password=decoy-pass; }; f`
	if err := exec.Command("git", "config", "-f", fakeGlobalConfig, "credential.helper", decoyHelper).Run(); err != nil {
		t.Fatalf("seed fake global config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", fakeGlobalConfig)

	ConfigureCredHelper(repoDir)

	cmd := exec.Command("git", "-C", repoDir, "credential", "fill")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=example.com\npath=owner/repo.git\n\n")
	cmd.Env = append(os.Environ(), "GIT_TOKEN=real-token-value")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git credential fill: %v", err)
	}

	got := string(out)
	if strings.Contains(got, "decoy") {
		t.Errorf("inherited (global) credential.helper won over the one ConfigureCredHelper set:\n%s", got)
	}
	if !strings.Contains(got, "username=oauth2") || !strings.Contains(got, "password=real-token-value") {
		t.Errorf("expected our helper's username=oauth2/password=real-token-value, got:\n%s", got)
	}
}
