package executor

import (
	"os"
	"path/filepath"
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
