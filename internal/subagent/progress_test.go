package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProgress_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got := ReadProgress(dir)
	if got != nil {
		t.Errorf("expected nil for missing file, got %v", got)
	}
}

func TestReadProgress_ValidFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`{"completed_steps":["1","3"]}`), 0644)

	got := ReadProgress(dir)
	if len(got) != 2 || got[0] != "1" || got[1] != "3" {
		t.Errorf("expected [1 3], got %v", got)
	}
}

func TestReadProgress_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`not json`), 0644)

	got := ReadProgress(dir)
	if got != nil {
		t.Errorf("expected nil for invalid JSON, got %v", got)
	}
}

func TestReadProgress_EmptyArray(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`{"completed_steps":[]}`), 0644)

	got := ReadProgress(dir)
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}
