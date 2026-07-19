package template

import (
	"strings"
	"testing"
)

func mkFile(filename string, size int, wellKnown bool) MemoryFile {
	return MemoryFile{
		Name:      deriveDisplayName(strings.TrimSuffix(filename, ".md")),
		Filename:  filename,
		Content:   strings.Repeat("x", size),
		WellKnown: wellKnown,
	}
}

func TestApplyBudget_UnlimitedWhenZero(t *testing.T) {
	r := Retrieval{Files: []MemoryFile{mkFile("big.md", 100000, false)}}
	out, warnings := ApplyBudget(r, 0)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %v", warnings)
	}
	if out.Files[0].Content != r.Files[0].Content {
		t.Error("content modified despite unlimited budget")
	}
}

func TestApplyBudget_NoOpUnderBudget(t *testing.T) {
	r := Retrieval{Files: []MemoryFile{
		mkFile("user_preferences.md", 100, true),
		mkFile("notes.md", 100, false),
	}}
	out, warnings := ApplyBudget(r, 1000)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %v", warnings)
	}
	if len(out.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(out.Files))
	}
}

func TestApplyBudget_PerFileCapTruncatesWithMarker(t *testing.T) {
	r := Retrieval{Files: []MemoryFile{
		mkFile("user_preferences.md", 100, true),
		mkFile("huge.md", 9000, false),
	}}
	out, warnings := ApplyBudget(r, 3000)
	var huge MemoryFile
	for _, f := range out.Files {
		if f.Filename == "huge.md" {
			huge = f
		}
	}
	if huge.Filename == "" {
		t.Fatal("huge.md was dropped, expected truncation")
	}
	if !strings.Contains(huge.Content, "truncated") || !strings.Contains(huge.Content, "huge.md") {
		t.Errorf("expected truncation marker naming the file, got tail %q", huge.Content[len(huge.Content)-80:])
	}
	if len(warnings) == 0 {
		t.Error("expected warnings")
	}
}

func TestApplyBudget_DropsMiscBeforeWellKnown(t *testing.T) {
	r := Retrieval{Files: []MemoryFile{
		mkFile("user_preferences.md", 500, true),
		mkFile("decisions.md", 500, true),
		mkFile("misc_a.md", 500, false),
		mkFile("misc_b.md", 500, false),
	}}
	out, _ := ApplyBudget(r, 1100)
	for _, f := range out.Files {
		if !f.WellKnown && f.Filename == "misc_b.md" {
			t.Error("misc_b.md (last-loaded) should be dropped first")
		}
	}
	// Well-known files must survive as entries.
	found := 0
	for _, f := range out.Files {
		if f.WellKnown {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected both well-known files to survive, got %d", found)
	}
}

func TestApplyBudget_WellKnownTruncatedInReverseOrder(t *testing.T) {
	r := Retrieval{Files: []MemoryFile{
		mkFile("user_preferences.md", 400, true),
		mkFile("workflows.md", 400, true),
	}}
	out, _ := ApplyBudget(r, 500)
	if total := totalSize(out); total > 500 {
		t.Errorf("total %d exceeds budget 500", total)
	}
	// user_preferences (first) should be intact; workflows (last) trimmed.
	if !strings.HasPrefix(out.Files[0].Content, strings.Repeat("x", 400)) {
		t.Error("user_preferences.md should be trimmed last / survive intact here")
	}
	if !strings.Contains(out.Files[1].Content, "truncated") {
		t.Error("workflows.md should carry a truncation marker")
	}
}

func TestApplyBudget_DoesNotMutateInput(t *testing.T) {
	orig := mkFile("huge.md", 9000, false)
	r := Retrieval{Files: []MemoryFile{orig}}
	ApplyBudget(r, 300)
	if r.Files[0].Content != orig.Content {
		t.Error("input retrieval was mutated")
	}
}
