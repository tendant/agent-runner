package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDated(t *testing.T, dir string, daysAgo int, host, content string) string {
	t.Helper()
	date := time.Now().AddDate(0, 0, -daysAgo).Format("2006-01-02")
	name := date + "-" + host + ".md"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return date
}

func TestRecentSessions_WindowFiltering(t *testing.T) {
	dir := t.TempDir()
	today := writeDated(t, dir, 0, "hosta", "today entry")
	writeDated(t, dir, 10, "hosta", "old entry")

	out := RecentSessions(dir, 7, 0)
	if !strings.Contains(out, "today entry") {
		t.Error("expected today's entry")
	}
	if strings.Contains(out, "old entry") {
		t.Error("entry outside the 7-day window should be excluded")
	}
	if !strings.Contains(out, "### "+today+" (hosta)") {
		t.Errorf("expected date header, got:\n%s", out)
	}
}

func TestRecentSessions_NewestFirst(t *testing.T) {
	dir := t.TempDir()
	writeDated(t, dir, 2, "h", "older-day")
	writeDated(t, dir, 0, "h", "newest-day")

	out := RecentSessions(dir, 7, 0)
	if strings.Index(out, "newest-day") > strings.Index(out, "older-day") {
		t.Errorf("expected newest first:\n%s", out)
	}
}

func TestRecentSessions_BudgetDropsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	writeDated(t, dir, 1, "h", strings.Repeat("o", 200))
	writeDated(t, dir, 0, "h", strings.Repeat("n", 200))

	out := RecentSessions(dir, 7, 300)
	if !strings.Contains(out, "nnn") {
		t.Error("newest day should survive the budget")
	}
	if strings.Contains(out, "ooo") {
		t.Error("oldest day should be dropped by the budget")
	}
}

func TestRecentSessions_SingleOversizedDayTruncated(t *testing.T) {
	dir := t.TempDir()
	writeDated(t, dir, 0, "h", strings.Repeat("x", 1000))

	out := RecentSessions(dir, 7, 300)
	if out == "" {
		t.Fatal("expected truncated content, got empty")
	}
	if len(out) > 400 { // budget + truncation notice slack
		t.Errorf("expected roughly budget-sized output, got %d chars", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Error("expected truncation marker")
	}
}

func TestRecentSessions_EmptyCases(t *testing.T) {
	dir := t.TempDir()
	if out := RecentSessions(dir, 7, 0); out != "" {
		t.Errorf("empty dir should yield empty, got %q", out)
	}
	writeDated(t, dir, 0, "h", "entry")
	if out := RecentSessions(dir, 0, 0); out != "" {
		t.Errorf("days=0 should yield empty, got %q", out)
	}
	if out := RecentSessions("", 7, 0); out != "" {
		t.Errorf("empty dir path should yield empty, got %q", out)
	}
}

func TestCompile_RecentSessionsOrdering(t *testing.T) {
	out := Compile(PromptInput{
		SystemInstructions: "SYSTEM",
		Retrieval:          Retrieval{Files: []MemoryFile{{Name: "Decisions", Content: "decision body"}}},
		RecentSessions:     "session {{MESSAGE}} body",
		CurrentRequest:     "do the thing",
		Vars:               map[string]string{"MESSAGE": "SUBSTITUTED"},
	})
	iMem := strings.Index(out, "## Decisions")
	iSess := strings.Index(out, "## Recent Sessions")
	iReq := strings.Index(out, "## Current Request")
	if !(iMem < iSess && iSess < iReq) {
		t.Errorf("section order wrong: mem=%d sess=%d req=%d\n%s", iMem, iSess, iReq, out)
	}
	if strings.Contains(out, "session SUBSTITUTED body") {
		t.Error("RecentSessions must not receive var substitution")
	}
}
