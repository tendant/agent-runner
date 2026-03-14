package template

import "testing"

func TestParseFrontmatter(t *testing.T) {
	input := `---
title: Agent Identity
summary: Defines who the agent is
read_when: always
priority: 10
---

# Identity

You are a helpful autonomous agent.`

	meta, body := ParseFrontmatter(input)

	if meta.Title != "Agent Identity" {
		t.Errorf("title = %q, want %q", meta.Title, "Agent Identity")
	}
	if meta.Summary != "Defines who the agent is" {
		t.Errorf("summary = %q", meta.Summary)
	}
	if meta.ReadWhen != "always" {
		t.Errorf("read_when = %q", meta.ReadWhen)
	}
	if meta.Priority != 10 {
		t.Errorf("priority = %d, want 10", meta.Priority)
	}
	if !contains(body, "# Identity") || !contains(body, "helpful autonomous agent") {
		t.Errorf("body = %q", body)
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	input := "# Just a plain file\n\nNo frontmatter here."
	meta, body := ParseFrontmatter(input)

	if meta.Title != "" || meta.Priority != 0 {
		t.Errorf("expected empty meta, got %+v", meta)
	}
	if body != input {
		t.Errorf("body should equal input verbatim")
	}
}

func TestParseFrontmatter_UnclosedDelimiter(t *testing.T) {
	input := "---\ntitle: Broken\nNo closing delimiter"
	_, body := ParseFrontmatter(input)
	if body != input {
		t.Errorf("unclosed frontmatter should return full content")
	}
}

func TestParseFrontmatter_EmptyBody(t *testing.T) {
	input := "---\ntitle: Minimal\n---"
	meta, body := ParseFrontmatter(input)
	if meta.Title != "Minimal" {
		t.Errorf("title = %q", meta.Title)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
