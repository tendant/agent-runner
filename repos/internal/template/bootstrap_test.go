package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsFirstRun_EmptyDir(t *testing.T) {
	if IsFirstRun("") {
		t.Error("should be false for empty dir")
	}
}

func TestIsFirstRun_NoBootstrap(t *testing.T) {
	dir := t.TempDir()
	if IsFirstRun(dir) {
		t.Error("should be false when BOOTSTRAP.md doesn't exist")
	}
}

func TestIsFirstRun_WithBootstrap(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "BOOTSTRAP.md"), []byte("# First run"), 0644)
	if !IsFirstRun(dir) {
		t.Error("should be true when BOOTSTRAP.md exists")
	}
}

func TestCompleteBootstrap_Renames(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "BOOTSTRAP.md")
	os.WriteFile(src, []byte("# Bootstrap"), 0644)

	err := CompleteBootstrap(dir)
	if err != nil {
		t.Fatalf("CompleteBootstrap error: %v", err)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("BOOTSTRAP.md should no longer exist")
	}

	dst := filepath.Join(dir, "BOOTSTRAP.md.done")
	if _, err := os.Stat(dst); err != nil {
		t.Error("BOOTSTRAP.md.done should exist")
	}
}

func TestCompleteBootstrap_NoOp(t *testing.T) {
	dir := t.TempDir()
	err := CompleteBootstrap(dir)
	if err != nil {
		t.Errorf("should be no-op when BOOTSTRAP.md doesn't exist, got: %v", err)
	}
}

func TestCompleteBootstrap_EmptyDir(t *testing.T) {
	err := CompleteBootstrap("")
	if err != nil {
		t.Errorf("should be no-op for empty dir, got: %v", err)
	}
}
