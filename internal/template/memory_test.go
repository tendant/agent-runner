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
		t.Errorf("expected empty for no memoryDir, got %q", result)
	}
}

func TestComposeMemorySection_NoFiles(t *testing.T) {
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
	memDir := t.TempDir()

	os.WriteFile(filepath.Join(memDir, "2026-03-01.md"), []byte("Did task A"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-02.md"), []byte("Did task B"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-03.md"), []byte("Did task C"), 0644)

	result := ComposeMemorySection(memDir, 2)
	// Should only include last 2 days
	if strings.Contains(result, "Did task A") {
		t.Error("should not include oldest log when days=2")
	}
	if !strings.Contains(result, "Did task B") || !strings.Contains(result, "Did task C") {
		t.Error("should include last 2 daily logs")
	}
}

func TestComposeMemorySection_CombinedMemoryAndLogs(t *testing.T) {
	memDir := t.TempDir()
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("Curated memory"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-03.md"), []byte("Today's log"), 0644)

	result := ComposeMemorySection(memDir, 7)
	if !strings.Contains(result, "Curated memory") {
		t.Error("should contain MEMORY.md")
	}
	if !strings.Contains(result, "Today's log") {
		t.Error("should contain daily log")
	}
}

func TestAppendDailyLog_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	err := AppendDailyLog(dir, "test entry 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify dir was created
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatal("memory dir should be created")
	}

	// Verify file exists with today's date
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	if !strings.HasSuffix(entries[0].Name(), ".md") {
		t.Error("file should have .md extension")
	}

	// Read content
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

func TestSeedMemory_CreatesFromTemplate(t *testing.T) {
	tmplDir := t.TempDir()
	os.WriteFile(filepath.Join(tmplDir, "MEMORY.md"), []byte("# Initial memory\n\nSeed content."), 0644)

	memDir := filepath.Join(t.TempDir(), "memory")
	err := SeedMemory(tmplDir, memDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(memDir, "MEMORY.md"))
	if err != nil {
		t.Fatalf("MEMORY.md should exist in memory dir: %v", err)
	}
	if !strings.Contains(string(data), "Seed content") {
		t.Error("should contain seeded content")
	}
}

func TestSeedMemory_NoOpIfExists(t *testing.T) {
	tmplDir := t.TempDir()
	os.WriteFile(filepath.Join(tmplDir, "MEMORY.md"), []byte("Template content"), 0644)

	memDir := t.TempDir()
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("Existing content"), 0644)

	err := SeedMemory(tmplDir, memDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not overwrite
	data, _ := os.ReadFile(filepath.Join(memDir, "MEMORY.md"))
	if strings.Contains(string(data), "Template content") {
		t.Error("should not overwrite existing memory")
	}
	if !strings.Contains(string(data), "Existing content") {
		t.Error("should preserve existing content")
	}
}

func TestSeedMemory_NoTemplateMemory(t *testing.T) {
	tmplDir := t.TempDir() // No MEMORY.md
	memDir := filepath.Join(t.TempDir(), "memory")

	err := SeedMemory(tmplDir, memDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Dir should be created but no MEMORY.md
	if _, err := os.Stat(memDir); os.IsNotExist(err) {
		t.Fatal("memory dir should be created")
	}
	if _, err := os.Stat(filepath.Join(memDir, "MEMORY.md")); !os.IsNotExist(err) {
		t.Error("MEMORY.md should not exist when template has none")
	}
}

func TestSeedMemory_EmptyDirs(t *testing.T) {
	if err := SeedMemory("", ""); err != nil {
		t.Errorf("expected nil for empty dirs, got %v", err)
	}
	if err := SeedMemory("", "/tmp/some-dir"); err != nil {
		t.Errorf("expected nil for empty templatesDir, got %v", err)
	}
}

func TestComposeMemorySection_InvalidFilenames(t *testing.T) {
	memDir := t.TempDir()

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Not a date"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-03.md"), []byte("Valid"), 0644)

	result := ComposeMemorySection(memDir, 7)
	if strings.Contains(result, "Not a date") {
		t.Error("should skip non-date filenames")
	}
	if !strings.Contains(result, "Valid") {
		t.Error("should include valid date file")
	}
}
