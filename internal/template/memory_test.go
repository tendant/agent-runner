package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeMemorySection_Empty(t *testing.T) {
	result := ComposeMemorySection("", 7)
	if result != "" {
		t.Errorf("expected empty for no templatesDir, got %q", result)
	}
}

func TestComposeMemorySection_NoMemoryDir(t *testing.T) {
	dir := t.TempDir()
	result := ComposeMemorySection(dir, 7)
	if result != "" {
		t.Errorf("expected empty when no memory files exist, got %q", result)
	}
}

func TestComposeMemorySection_MemoryMDOnly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("# Long-term memory\n\nImportant fact."), 0644)

	result := ComposeMemorySection(dir, 7)
	if !strings.Contains(result, "## Memory") {
		t.Error("should have Memory header")
	}
	if !strings.Contains(result, "Important fact") {
		t.Error("should contain MEMORY.md content")
	}
}

func TestComposeMemorySection_DailyLogs(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	os.WriteFile(filepath.Join(memDir, "2026-03-01.md"), []byte("Did task A"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-02.md"), []byte("Did task B"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-03.md"), []byte("Did task C"), 0644)

	result := ComposeMemorySection(dir, 2)
	// Should only include last 2 days
	if strings.Contains(result, "Did task A") {
		t.Error("should not include oldest log when days=2")
	}
	if !strings.Contains(result, "Did task B") || !strings.Contains(result, "Did task C") {
		t.Error("should include last 2 daily logs")
	}
}

func TestComposeMemorySection_CombinedMemoryAndLogs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("Curated memory"), 0644)

	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	os.WriteFile(filepath.Join(memDir, "2026-03-03.md"), []byte("Today's log"), 0644)

	result := ComposeMemorySection(dir, 7)
	if !strings.Contains(result, "Curated memory") {
		t.Error("should contain MEMORY.md")
	}
	if !strings.Contains(result, "Today's log") {
		t.Error("should contain daily log")
	}
}

func TestComposeMemorySection_InvalidFilenames(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Not a date"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-03.md"), []byte("Valid"), 0644)

	result := ComposeMemorySection(dir, 7)
	if strings.Contains(result, "Not a date") {
		t.Error("should skip non-date filenames")
	}
	if !strings.Contains(result, "Valid") {
		t.Error("should include valid date file")
	}
}
