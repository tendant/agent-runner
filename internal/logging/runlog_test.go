package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateMarkdown_SuccessfulRun(t *testing.T) {
	l := NewRunLogger("/tmp/runs")

	data := &RunLogData{
		JobID:       "job-123",
		Project:     "my-project",
		Status:      "completed",
		Duration:    42,
		Instruction: "Fix the login bug",
		ChangedFiles: []FileChange{
			{Path: "auth.go", Insertions: 10, Deletions: 5},
			{Path: "auth_test.go", Insertions: 20, Deletions: 2},
		},
		DiffSummary: DiffSummary{Insertions: 30, Deletions: 7},
		Commit:      "abc1234",
		Branch:      "main",
		ValidationOK: true,
	}

	md := l.generateMarkdown(data, "2024-01-15_10-30-00")

	if !strings.Contains(md, "# Claude Run - 2024-01-15 10-30-00") {
		t.Error("expected header with formatted timestamp")
	}
	if !strings.Contains(md, "**Job ID:** job-123") {
		t.Error("expected job ID")
	}
	if !strings.Contains(md, "**Project:** my-project") {
		t.Error("expected project name")
	}
	if !strings.Contains(md, "**Status:** completed") {
		t.Error("expected status")
	}
	if !strings.Contains(md, "**Duration:** 42s") {
		t.Error("expected duration")
	}
	if !strings.Contains(md, "> Fix the login bug") {
		t.Error("expected instruction")
	}
	if !strings.Contains(md, "`auth.go` (+10, -5)") {
		t.Error("expected changed file with stats")
	}
	if !strings.Contains(md, "Insertions: 30") {
		t.Error("expected diff summary insertions")
	}
	if !strings.Contains(md, "Deletions: 7") {
		t.Error("expected diff summary deletions")
	}
	if !strings.Contains(md, "`abc1234` pushed to `origin/main`") {
		t.Error("expected commit info")
	}
	if !strings.Contains(md, "✓ All changes within allowed paths") {
		t.Error("expected validation passed")
	}
}

func TestGenerateMarkdown_FailedRun(t *testing.T) {
	l := NewRunLogger("/tmp/runs")

	data := &RunLogData{
		JobID:       "job-456",
		Project:     "my-project",
		Status:      "failed",
		Instruction: "Do something",
		Error:       "CLAUDE_ERROR: process exited with code 1",
		ErrorCode:   "CLAUDE_ERROR",
	}

	md := l.generateMarkdown(data, "2024-01-15_10-30-00")

	if !strings.Contains(md, "**Status:** failed") {
		t.Error("expected failed status")
	}
	if !strings.Contains(md, "## Error") {
		t.Error("expected error section")
	}
	if !strings.Contains(md, "**Code:** `CLAUDE_ERROR`") {
		t.Error("expected error code")
	}
	if !strings.Contains(md, "CLAUDE_ERROR: process exited with code 1") {
		t.Error("expected error message")
	}
}

func TestGenerateMarkdown_ValidationFailure(t *testing.T) {
	l := NewRunLogger("/tmp/runs")

	data := &RunLogData{
		JobID:       "job-789",
		Project:     "my-project",
		Status:      "failed",
		Instruction: "Modify CI",
		ValidationOK: false,
		ValidationError: &ValidationResult{
			Code:    "CI_CONFIG_VIOLATION",
			Message: "Attempted to modify CI configuration",
			Files:   []string{".github/workflows/ci.yml"},
		},
	}

	md := l.generateMarkdown(data, "2024-01-15_10-30-00")

	if !strings.Contains(md, "✗ **CI_CONFIG_VIOLATION**") {
		t.Error("expected validation error code")
	}
	if !strings.Contains(md, "Attempted to modify CI configuration") {
		t.Error("expected validation error message")
	}
	if !strings.Contains(md, "`.github/workflows/ci.yml`") {
		t.Error("expected violating file")
	}
}

func TestGenerateMarkdown_ExecutionLog(t *testing.T) {
	l := NewRunLogger("/tmp/runs")

	data := &RunLogData{
		JobID:        "job-1",
		Project:      "test",
		Status:       "completed",
		Instruction:  "Do work",
		ValidationOK: true,
		ExecutionLog: "Claude Code output here\nLine 2",
	}

	md := l.generateMarkdown(data, "2024-01-15_10-30-00")

	if !strings.Contains(md, "## Execution Log") {
		t.Error("expected execution log section")
	}
	if !strings.Contains(md, "<details>") {
		t.Error("expected details tag")
	}
	if !strings.Contains(md, "Claude Code output here") {
		t.Error("expected execution log content")
	}
}

func TestGenerateMarkdown_MinimalData(t *testing.T) {
	l := NewRunLogger("/tmp/runs")

	data := &RunLogData{
		JobID:       "job-1",
		Project:     "test",
		Status:      "completed",
		Instruction: "Simple task",
		ValidationOK: true,
	}

	md := l.generateMarkdown(data, "2024-01-15_10-30-00")

	// Should not have changed files or diff summary sections
	if strings.Contains(md, "## Changed Files") {
		t.Error("should not have changed files section with no files")
	}
	if strings.Contains(md, "## Diff Summary") {
		t.Error("should not have diff summary section with zero values")
	}
	if strings.Contains(md, "## Error") {
		t.Error("should not have error section")
	}
	if strings.Contains(md, "## Commit") {
		t.Error("should not have commit section with no commit")
	}
	// Should not contain duration if 0
	if strings.Contains(md, "**Duration:**") {
		t.Error("should not have duration when 0")
	}
}

func TestGenerateMarkdown_DefaultBranch(t *testing.T) {
	l := NewRunLogger("/tmp/runs")

	data := &RunLogData{
		JobID:        "job-1",
		Project:      "test",
		Status:       "completed",
		Instruction:  "Task",
		Commit:       "abc",
		Branch:       "", // empty branch should default to main
		ValidationOK: true,
	}

	md := l.generateMarkdown(data, "2024-01-15_10-30-00")

	if !strings.Contains(md, "origin/main") {
		t.Error("expected default branch 'main' when branch is empty")
	}
}

func TestGenerateMarkdown_FileWithoutStats(t *testing.T) {
	l := NewRunLogger("/tmp/runs")

	data := &RunLogData{
		JobID:       "job-1",
		Project:     "test",
		Status:      "completed",
		Instruction: "Task",
		ChangedFiles: []FileChange{
			{Path: "new_file.go", Insertions: 0, Deletions: 0},
		},
		ValidationOK: true,
	}

	md := l.generateMarkdown(data, "2024-01-15_10-30-00")

	if !strings.Contains(md, "- `new_file.go`\n") {
		t.Error("expected file without stats")
	}
	// Should NOT have (+0, -0)
	if strings.Contains(md, "(+0, -0)") {
		t.Error("should not show zero stats")
	}
}

func TestWriteRunLog_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	l := NewRunLogger(dir)

	data := &RunLogData{
		JobID:        "job-1",
		Project:      "test-project",
		Status:       "completed",
		Instruction:  "Do something",
		ValidationOK: true,
	}

	path, err := l.WriteRunLog(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasSuffix(path, "_test-project.md") {
		t.Errorf("expected filename ending in _test-project.md, got %s", filepath.Base(path))
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}

	if !strings.Contains(string(content), "job-1") {
		t.Error("expected file to contain job ID")
	}
}

func TestWriteRunLog_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	runsDir := filepath.Join(dir, "nested", "runs")
	l := NewRunLogger(runsDir)

	data := &RunLogData{
		JobID:        "job-1",
		Project:      "test",
		Status:       "completed",
		Instruction:  "Task",
		ValidationOK: true,
	}

	path, err := l.WriteRunLog(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("expected file to exist")
	}
}
