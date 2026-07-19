package curator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLLM implements llm.Client with a canned response.
type fakeLLM struct {
	response string
	err      error
	prompt   string // captures the last prompt
}

func (f *fakeLLM) Complete(_ context.Context, prompt string) (string, error) {
	f.prompt = prompt
	return f.response, f.err
}

func writeMem(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readMem(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestCurate_AppendsLessonAndCompacts(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("duplicate decision\n", 300) // ~5700 chars, over 12000/3=4000
	writeMem(t, dir, "decisions.md", big)

	fake := &fakeLLM{response: `{"lessons_append": "- prefer table tests", "compact": [{"file": "decisions.md", "content": "duplicate decision (deduped)"}]}`}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	sum, err := c.Curate(context.Background(), Input{TaskPreview: "task", Status: "completed"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sum.LessonAppended {
		t.Error("expected lesson appended")
	}
	if len(sum.FilesCompacted) != 1 || sum.FilesCompacted[0] != "decisions.md" {
		t.Errorf("expected decisions.md compacted, got %v", sum.FilesCompacted)
	}
	if got := readMem(t, dir, "lessons.md"); !strings.Contains(got, "prefer table tests") {
		t.Errorf("lessons.md missing lesson: %q", got)
	}
	if got := readMem(t, dir, "decisions.md"); !strings.Contains(got, "deduped") || len(got) > 100 {
		t.Errorf("decisions.md not compacted: %d chars", len(got))
	}
	if !strings.Contains(fake.prompt, "OVER BUDGET") {
		t.Error("prompt should flag decisions.md as over budget")
	}
}

func TestCurate_RejectsCompactionOfFileNotOverBudget(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "decisions.md", "small")

	fake := &fakeLLM{response: `{"lessons_append": "", "compact": [{"file": "decisions.md", "content": ""}]}`}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	sum, err := c.Curate(context.Background(), Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sum.FilesCompacted) != 0 {
		t.Errorf("expected no compaction, got %v", sum.FilesCompacted)
	}
	if got := readMem(t, dir, "decisions.md"); got != "small" {
		t.Errorf("decisions.md was modified: %q", got)
	}
}

func TestCurate_RejectsLargerRewrite(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 5000)
	writeMem(t, dir, "workflows.md", big)

	fake := &fakeLLM{response: `{"lessons_append": "", "compact": [{"file": "workflows.md", "content": "` + strings.Repeat("y", 6000) + `"}]}`}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	sum, err := c.Curate(context.Background(), Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sum.FilesCompacted) != 0 {
		t.Errorf("expected larger rewrite rejected, got %v", sum.FilesCompacted)
	}
	if got := readMem(t, dir, "workflows.md"); got != big {
		t.Error("workflows.md was modified by a larger rewrite")
	}
}

func TestCurate_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "agent.md", "IDENTITY")

	fake := &fakeLLM{response: `{"lessons_append": "", "compact": [{"file": "../agent.md", "content": "pwned"}, {"file": "agent.md", "content": "pwned"}]}`}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	sum, err := c.Curate(context.Background(), Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sum.FilesCompacted) != 0 {
		t.Errorf("expected all compactions rejected, got %v", sum.FilesCompacted)
	}
	if got := readMem(t, dir, "agent.md"); got != "IDENTITY" {
		t.Error("agent.md was modified — allowlist breached")
	}
}

func TestCurate_GarbageOutputLeavesMemoryUntouched(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "decisions.md", "content")

	fake := &fakeLLM{response: "I could not produce JSON, sorry!"}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	if _, err := c.Curate(context.Background(), Input{}); err == nil {
		t.Fatal("expected parse error")
	}
	if got := readMem(t, dir, "decisions.md"); got != "content" {
		t.Error("memory modified despite parse failure")
	}
	if readMem(t, dir, "lessons.md") != "" {
		t.Error("lessons.md created despite parse failure")
	}
}

func TestCurate_JSONWrappedInProse(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLLM{response: "Here's my analysis:\n{\"lessons_append\": \"- lesson\", \"compact\": []}\nDone."}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	sum, err := c.Curate(context.Background(), Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sum.LessonAppended {
		t.Error("expected lesson extracted from prose-wrapped JSON")
	}
}

func TestCurate_LessonAppendedToExisting(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "lessons.md", "# Lessons\n\n### 2026-01-01\n- old lesson\n")

	fake := &fakeLLM{response: `{"lessons_append": "- new lesson", "compact": []}`}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	if _, err := c.Curate(context.Background(), Input{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readMem(t, dir, "lessons.md")
	if !strings.Contains(got, "old lesson") || !strings.Contains(got, "new lesson") {
		t.Errorf("expected both lessons present:\n%s", got)
	}
}

func TestCurate_ReviewFindingsInPrompt(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLLM{response: `{"lessons_append": "", "compact": []}`}
	c := New(fake, Config{MemoryDir: dir, CharCap: 12000})

	_, err := c.Curate(context.Background(), Input{
		TaskPreview: "fix the parser",
		Status:      "completed",
		HasReview:   true,
		ReviewScore: 6,
		ReviewIssues: []string{"missing error handling"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"fix the parser", "6/10", "missing error handling"} {
		if !strings.Contains(fake.prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
