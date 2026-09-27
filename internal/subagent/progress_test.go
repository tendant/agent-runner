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

func TestPlanPersistence(t *testing.T) {
	dir := t.TempDir()
	if LoadPlan(dir) != nil {
		t.Fatal("LoadPlan on an empty task dir returned a plan")
	}
	plan := &PlanResult{Summary: "s", Steps: []PlanStep{{ID: "1", Description: "build"}, {ID: "2", Description: "ship"}}}
	if err := SavePlan(dir, plan); err != nil {
		t.Fatal(err)
	}
	got := LoadPlan(dir)
	if got == nil || len(got.Steps) != 2 || got.Steps[1].Description != "ship" {
		t.Fatalf("LoadPlan = %+v", got)
	}
}

func TestResetTurnFieldsKeepsCompletedSteps(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`{"completed_steps":["1"],"blocked_steps":[{"step":"2","reason":"x"}],"status":"needs_input","question":"q?","summary":"s","decisions":["d"]}`), 0o644)
	if err := ResetTurnFields(dir); err != nil {
		t.Fatal(err)
	}
	p := ReadProgress(dir)
	if len(p.CompletedSteps) != 1 || p.Status != "" || p.Question != "" || p.Summary != "" || len(p.Decisions) != 0 || len(p.BlockedSteps) != 0 {
		t.Fatalf("after reset: %+v", p)
	}
	if err := ResetTurnFields(t.TempDir()); err != nil {
		t.Fatalf("reset with no progress file: %v", err)
	}
}

func TestSyncCompletedSteps(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`{"completed_steps":["1","2"],"summary":"s"}`), 0o644)
	plan := &PlanResult{Steps: []PlanStep{{ID: "1"}, {ID: "2", Done: true}, {ID: "3"}}}
	if err := SyncCompletedSteps(dir, plan); err != nil {
		t.Fatal(err)
	}
	p := ReadProgress(dir)
	if len(p.CompletedSteps) != 1 || p.CompletedSteps[0] != "2" || p.Summary != "s" {
		t.Fatalf("after sync: %+v", p)
	}
}
