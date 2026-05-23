package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteMemoryFile_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	if err := WriteMemoryFile(dir, "notes.md", "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatal("memory dir should be created")
	}
	data, err := os.ReadFile(filepath.Join(dir, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", string(data), "hello")
	}
}

func TestReadMemoryFile_ReturnsContent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("content here"), 0644)

	got, err := ReadMemoryFile(dir, "test.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "content here" {
		t.Errorf("got %q, want %q", got, "content here")
	}
}

func TestReadMemoryFile_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadMemoryFile(dir, "nonexistent.md")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestAppendDailyLog_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	err := AppendDailyLog(dir, "test entry 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatal("memory dir should be created")
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}

	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if !strings.Contains(string(data), "test entry 1") {
		t.Error("should contain the entry")
	}
}

func TestAppendDailyLog_AppendsMultiple(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	AppendDailyLog(dir, "entry A")
	AppendDailyLog(dir, "entry B")

	entries, _ := os.ReadDir(dir)
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	content := string(data)
	if !strings.Contains(content, "entry A") || !strings.Contains(content, "entry B") {
		t.Error("should contain both entries")
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}

func TestAppendDailyLog_EmptyDir(t *testing.T) {
	err := AppendDailyLog("", "should be noop")
	if err != nil {
		t.Errorf("expected nil for empty memoryDir, got %v", err)
	}
}

// TestAppendDailyLog_HostnameSuffix verifies that AppendDailyLog creates files
// with the YYYY-MM-DD-<hostname>.md pattern.
func TestAppendDailyLog_HostnameSuffix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	if err := AppendDailyLog(dir, "hostname test entry"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	name := entries[0].Name()
	// Must be at least YYYY-MM-DD-x.md (len >= 15)
	if len(name) < 15 {
		t.Errorf("expected hostname-suffixed filename, got %q", name)
	}
	// First 10 chars must be a valid date
	stem := strings.TrimSuffix(name, ".md")
	if len(stem) < 11 || stem[10] != '-' {
		t.Errorf("expected YYYY-MM-DD-hostname format, got %q", name)
	}
}
