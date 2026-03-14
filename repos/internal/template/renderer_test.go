package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender_VariableSubstitution(t *testing.T) {
	templates := []TemplateFile{
		{
			Name: "TEST.md",
			Meta: TemplateMeta{Title: "Test"},
			Body: "Message: {{MESSAGE}}\nRepos: {{REPOS}}\nDate: {{DATE}}\nIteration: {{ITERATION}}",
		},
	}

	ctx := TemplateContext{
		Message:   "build a website",
		Repos:     "site-a, site-b",
		Date:      "2026-03-03",
		Iteration: 5,
	}

	result := Render(templates, ctx)
	if !strings.Contains(result, "build a website") {
		t.Error("MESSAGE not substituted")
	}
	if !strings.Contains(result, "site-a, site-b") {
		t.Error("REPOS not substituted")
	}
	if !strings.Contains(result, "2026-03-03") {
		t.Error("DATE not substituted")
	}
	if !strings.Contains(result, "Iteration: 5") {
		t.Error("ITERATION not substituted")
	}
}

func TestRender_MultipleTemplates(t *testing.T) {
	templates := []TemplateFile{
		{Name: "A.md", Meta: TemplateMeta{Title: "First"}, Body: "Section A"},
		{Name: "B.md", Meta: TemplateMeta{Title: "Second"}, Body: "Section B"},
	}

	result := Render(templates, TemplateContext{})
	if !strings.Contains(result, "Section A") || !strings.Contains(result, "Section B") {
		t.Error("both sections should be present")
	}
	// Sections should be separated
	idxA := strings.Index(result, "Section A")
	idxB := strings.Index(result, "Section B")
	if idxA >= idxB {
		t.Error("Section A should come before Section B")
	}
}

func TestComposePrompt_DefaultsOnly(t *testing.T) {
	ctx := TemplateContext{
		Message:   "test task",
		Date:      "2026-03-03",
		Iteration: 1,
	}

	// Empty templates dir — should use only embedded defaults
	result, err := ComposePrompt("", PhaseBoot, false, ctx)
	if err != nil {
		t.Fatalf("ComposePrompt error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty prompt from defaults")
	}
	// Should include always + boot templates
	if !strings.Contains(result, "autonomous") {
		t.Error("should contain identity content")
	}
}

func TestComposePrompt_WithOverride(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "SOUL.md", `---
title: Override Soul
read_when: always
priority: 20
---
Custom soul content here.`)

	ctx := TemplateContext{Message: "test", Date: "2026-03-03", Iteration: 1}
	result, err := ComposePrompt(dir, PhaseBoot, false, ctx)
	if err != nil {
		t.Fatalf("ComposePrompt error: %v", err)
	}
	if !strings.Contains(result, "Custom soul content") {
		t.Error("override should replace default SOUL.md")
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := writeFile(dir, name, content); err != nil {
		t.Fatal(err)
	}
}

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
}
