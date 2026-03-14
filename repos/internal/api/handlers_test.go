package api

import (
	"testing"
)

func TestSummarizeAction_Add(t *testing.T) {
	result := summarizeAction("Add user authentication")
	if result != "Add feature" {
		t.Errorf("expected 'Add feature', got '%s'", result)
	}
}

func TestSummarizeAction_Fix(t *testing.T) {
	result := summarizeAction("Fix the login bug")
	if result != "Fix issue" {
		t.Errorf("expected 'Fix issue', got '%s'", result)
	}
}

func TestSummarizeAction_Update(t *testing.T) {
	result := summarizeAction("Update the README")
	if result != "Update" {
		t.Errorf("expected 'Update', got '%s'", result)
	}
}

func TestSummarizeAction_Remove(t *testing.T) {
	result := summarizeAction("Remove deprecated endpoint")
	if result != "Remove" {
		t.Errorf("expected 'Remove', got '%s'", result)
	}
}

func TestSummarizeAction_Delete(t *testing.T) {
	result := summarizeAction("Delete old migration files")
	if result != "Remove" {
		t.Errorf("expected 'Remove', got '%s'", result)
	}
}

func TestSummarizeAction_Refactor(t *testing.T) {
	result := summarizeAction("Refactor the authentication module")
	if result != "Refactor" {
		t.Errorf("expected 'Refactor', got '%s'", result)
	}
}

func TestSummarizeAction_Default_Short(t *testing.T) {
	result := summarizeAction("Do stuff")
	if result != "Do stuff" {
		t.Errorf("expected 'Do stuff', got '%s'", result)
	}
}

func TestSummarizeAction_Default_Long(t *testing.T) {
	result := summarizeAction("Implement a completely new feature that does many things")
	if result != "Implement a completely new" {
		t.Errorf("expected first 4 words, got '%s'", result)
	}
}

func TestSummarizeAction_CaseInsensitive(t *testing.T) {
	if summarizeAction("ADD feature") != "Add feature" {
		t.Error("should be case-insensitive for 'add'")
	}
	if summarizeAction("FIX bug") != "Fix issue" {
		t.Error("should be case-insensitive for 'fix'")
	}
}

func TestGenerateCommitMessage_FewFiles(t *testing.T) {
	h := &Handlers{}
	msg := h.generateCommitMessage(
		[]string{"main.go", "util.go"},
		"Fix the login bug",
	)

	if msg == "" {
		t.Fatal("expected non-empty commit message")
	}
	// Should contain file names
	if !contains(msg, "main.go") {
		t.Error("expected main.go in message")
	}
	if !contains(msg, "util.go") {
		t.Error("expected util.go in message")
	}
	// Should contain action
	if !contains(msg, "Fix issue") {
		t.Error("expected 'Fix issue' action summary")
	}
}

func TestGenerateCommitMessage_ManyFiles(t *testing.T) {
	h := &Handlers{}
	files := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	msg := h.generateCommitMessage(files, "Update all handlers")

	// Should show first 2 files and count
	if !contains(msg, "a.go") {
		t.Error("expected first file in message")
	}
	if !contains(msg, "b.go") {
		t.Error("expected second file in message")
	}
	if !contains(msg, "3 more files") {
		t.Error("expected '3 more files' truncation")
	}
}

func TestGenerateCommitMessage_LongInstruction(t *testing.T) {
	h := &Handlers{}
	longInstruction := "This is a very long instruction that exceeds one hundred characters and should be truncated in the commit message body to keep things readable"
	msg := h.generateCommitMessage([]string{"main.go"}, longInstruction)

	// The instruction in the message body should be truncated
	if !contains(msg, "...") {
		t.Error("expected truncation of long instruction")
	}
}

func TestGenerateCommitMessage_ShortInstruction(t *testing.T) {
	h := &Handlers{}
	msg := h.generateCommitMessage([]string{"main.go"}, "Fix bug")

	// Short instruction should not be truncated
	if contains(msg, "...") {
		t.Error("short instruction should not be truncated")
	}
	if !contains(msg, "Instruction: Fix bug") {
		t.Error("expected full instruction in message")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsString(s, substr))
}

func containsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
