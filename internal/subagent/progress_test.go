package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProgress_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got := ReadProgress(dir)
	if len(got.CompletedSteps) != 0 || len(got.BlockedSteps) != 0 {
		t.Errorf("expected empty result for missing file, got %+v", got)
	}
}

func TestReadProgress_ValidFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`{"completed_steps":["1","3"]}`), 0644)

	got := ReadProgress(dir)
	if len(got.CompletedSteps) != 2 || got.CompletedSteps[0] != "1" || got.CompletedSteps[1] != "3" {
		t.Errorf("expected [1 3], got %v", got.CompletedSteps)
	}
}

func TestReadProgress_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`not json`), 0644)

	got := ReadProgress(dir)
	if len(got.CompletedSteps) != 0 || len(got.BlockedSteps) != 0 {
		t.Errorf("expected empty result for invalid JSON, got %+v", got)
	}
}

func TestReadProgress_EmptyArray(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`{"completed_steps":[]}`), 0644)

	got := ReadProgress(dir)
	if len(got.CompletedSteps) != 0 {
		t.Errorf("expected empty completed steps, got %v", got.CompletedSteps)
	}
}

func TestReadProgress_BlockedSteps(t *testing.T) {
	dir := t.TempDir()
	content := `{"completed_steps":["1","2"],"blocked_steps":[{"step":"3","reason":"no images uploaded"}]}`
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(content), 0644)

	got := ReadProgress(dir)
	if len(got.CompletedSteps) != 2 {
		t.Errorf("expected 2 completed steps, got %d", len(got.CompletedSteps))
	}
	if len(got.BlockedSteps) != 1 || got.BlockedSteps[0].Step != "3" || got.BlockedSteps[0].Reason != "no images uploaded" {
		t.Errorf("unexpected blocked steps: %+v", got.BlockedSteps)
	}
}
