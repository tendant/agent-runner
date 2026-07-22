package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRetrieve_EmptyDir(t *testing.T) {
	r := Retrieve("")
	if len(r.Files) != 0 {
		t.Errorf("expected empty retrieval for empty dir, got %d files", len(r.Files))
	}
}

func TestRetrieve_MissingDir(t *testing.T) {
	r := Retrieve("/nonexistent/path/that/does/not/exist")
	if len(r.Files) != 0 {
		t.Errorf("expected empty retrieval for missing dir, got %d files", len(r.Files))
	}
}

func TestRetrieve_WellKnownFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	// Write all well-known files (in reverse order to verify output ordering).
	os.WriteFile(filepath.Join(dir, "workflows.md"), []byte("workflow content"), 0644)
	os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("decisions content"), 0644)
	os.WriteFile(filepath.Join(dir, "project_summary.md"), []byte("project summary content"), 0644)
	os.WriteFile(filepath.Join(dir, "user_preferences.md"), []byte("user prefs content"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 4 {
		t.Fatalf("expected 4 files, got %d", len(r.Files))
	}
	// Verify order and display names.
	expected := []struct {
		name    string
		content string
	}{
		{"User Preferences", "user prefs content"},
		{"Project Summary", "project summary content"},
		{"Decisions", "decisions content"},
		{"Workflows", "workflow content"},
	}
	for i, exp := range expected {
		if r.Files[i].Name != exp.name {
			t.Errorf("Files[%d].Name = %q, want %q", i, r.Files[i].Name, exp.name)
		}
		if r.Files[i].Content != exp.content {
			t.Errorf("Files[%d].Content = %q, want %q", i, r.Files[i].Content, exp.content)
		}
	}
}

func TestRetrieve_MissingWellKnownFilesSkipped(t *testing.T) {
	dir := t.TempDir()
	// Only write two of the four well-known files.
	os.WriteFile(filepath.Join(dir, "user_preferences.md"), []byte("user prefs"), 0644)
	os.WriteFile(filepath.Join(dir, "workflows.md"), []byte("workflows"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(r.Files))
	}
	// project_summary and decisions should not appear as empty sections.
	for _, f := range r.Files {
		if f.Content == "" {
			t.Errorf("file %q has empty content — should have been skipped", f.Name)
		}
	}
}

func TestRetrieve_ExtraLowercaseFilesIncluded(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "coding_standards.md"), []byte("use tabs"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(r.Files))
	}
	if r.Files[0].Name != "Coding standards" {
		t.Errorf("display name = %q, want %q", r.Files[0].Name, "Coding standards")
	}
	if r.Files[0].Content != "use tabs" {
		t.Errorf("content = %q, want %q", r.Files[0].Content, "use tabs")
	}
}

func TestRetrieve_ExtraFileWithHyphen(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "team-conventions.md"), []byte("always review"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(r.Files))
	}
	if r.Files[0].Name != "Team conventions" {
		t.Errorf("display name = %q, want %q", r.Files[0].Name, "Team conventions")
	}
}

func TestRetrieve_AgentMdSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "agent.md"), []byte("system instructions"), 0644)
	os.WriteFile(filepath.Join(dir, "user_preferences.md"), []byte("prefs"), 0644)

	r := Retrieve(dir)
	for _, f := range r.Files {
		if f.Name == "Agent" || f.Name == "agent" {
			t.Error("agent.md should be skipped")
		}
	}
}

func TestRetrieve_PromptMdSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("workflow"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 0 {
		t.Errorf("prompt.md should be skipped, got %d files", len(r.Files))
	}
}

func TestRetrieve_HeartbeatMdSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("config"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 0 {
		t.Errorf("HEARTBEAT.md should be skipped, got %d files", len(r.Files))
	}
}

func TestRetrieve_MemoryMdLoaded(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("long-term memory index"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 1 {
		t.Errorf("MEMORY.md should be loaded, got %d files", len(r.Files))
	}
}

func TestRetrieve_UppercaseFilesLoaded(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("identity"), 0644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("soul"), 0644)
	os.WriteFile(filepath.Join(dir, "user_preferences.md"), []byte("prefs"), 0644)

	r := Retrieve(dir)
	// All three files should be included: well-known first, then uppercase extras.
	if len(r.Files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(r.Files), r.Files)
	}
	if r.Files[0].Name != "User Preferences" {
		t.Errorf("expected User Preferences first, got %q", r.Files[0].Name)
	}
}

func TestRetrieve_DailyLogFilesSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "2026-05-23.md"), []byte("today's log"), 0644)
	os.WriteFile(filepath.Join(dir, "2026-05-23-hostname.md"), []byte("host log"), 0644)
	os.WriteFile(filepath.Join(dir, "user_preferences.md"), []byte("prefs"), 0644)

	r := Retrieve(dir)
	// Only user_preferences.md should be included.
	if len(r.Files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(r.Files), r.Files)
	}
	if r.Files[0].Name != "User Preferences" {
		t.Errorf("expected User Preferences, got %q", r.Files[0].Name)
	}
}

func TestRetrieve_WellKnownBeforeExtra(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "user_preferences.md"), []byte("prefs"), 0644)
	os.WriteFile(filepath.Join(dir, "extra_notes.md"), []byte("notes"), 0644)

	r := Retrieve(dir)
	if len(r.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(r.Files))
	}
	if r.Files[0].Name != "User Preferences" {
		t.Errorf("expected User Preferences first, got %q", r.Files[0].Name)
	}
	if r.Files[1].Name != "Extra notes" {
		t.Errorf("expected Extra notes second, got %q", r.Files[1].Name)
	}
}

func TestRetrieve_SkipsWelcomeFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "WELCOME.md"), []byte("greeting text"), 0644)
	os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("a decision"), 0644)

	r := Retrieve(dir)
	for _, f := range r.Files {
		if f.Filename == "WELCOME.md" {
			t.Error("WELCOME.md must not be injected into prompts")
		}
	}
	if len(r.Files) != 1 {
		t.Errorf("expected only decisions.md, got %d files", len(r.Files))
	}
}
