package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	files, err := LoadDefaults()
	if err != nil {
		t.Fatalf("LoadDefaults() error: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected embedded defaults, got none")
	}

	// Verify well-known files are present
	names := make(map[string]bool)
	for _, f := range files {
		names[f.Name] = true
	}
	for _, name := range []string{"IDENTITY.md", "SOUL.md", "AGENTS.md", "USER.md", "TOOLS.md", "BOOT.md", "HEARTBEAT.md"} {
		if !names[name] {
			t.Errorf("missing default template: %s", name)
		}
	}
}

func TestLoadDefaults_Priorities(t *testing.T) {
	files, _ := LoadDefaults()
	for _, f := range files {
		if f.Name == "IDENTITY.md" && f.Meta.Priority != 10 {
			t.Errorf("IDENTITY.md priority = %d, want 10", f.Meta.Priority)
		}
		if f.Name == "SOUL.md" && f.Meta.Priority != 20 {
			t.Errorf("SOUL.md priority = %d, want 20", f.Meta.Priority)
		}
	}
}

func TestLoadFromDir_NonExistent(t *testing.T) {
	files, err := LoadFromDir("/nonexistent/path")
	if err != nil {
		t.Fatalf("unexpected error for nonexistent dir: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil, got %d files", len(files))
	}
}

func TestLoadFromDir_Empty(t *testing.T) {
	files, err := LoadFromDir("")
	if err != nil {
		t.Fatalf("unexpected error for empty dir: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil, got %d files", len(files))
	}
}

func TestLoadFromDir_WithOverride(t *testing.T) {
	dir := t.TempDir()
	content := `---
title: Custom Soul
read_when: always
priority: 20
---

# My Custom Soul

Be creative and bold.`
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(content), 0644)

	files, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Meta.Title != "Custom Soul" {
		t.Errorf("title = %q, want Custom Soul", files[0].Meta.Title)
	}
}

func TestMergeTemplates_Override(t *testing.T) {
	defaults := []TemplateFile{
		{Name: "SOUL.md", Meta: TemplateMeta{Title: "Default Soul", Priority: 20, ReadWhen: "always"}, Body: "default"},
		{Name: "IDENTITY.md", Meta: TemplateMeta{Title: "Identity", Priority: 10, ReadWhen: "always"}, Body: "identity"},
	}
	overrides := []TemplateFile{
		{Name: "SOUL.md", Meta: TemplateMeta{Title: "Custom Soul", Priority: 20, ReadWhen: "always"}, Body: "custom"},
	}

	merged := MergeTemplates(defaults, overrides)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged, got %d", len(merged))
	}

	for _, f := range merged {
		if f.Name == "SOUL.md" && f.Body != "custom" {
			t.Errorf("SOUL.md should be overridden, got body=%q", f.Body)
		}
	}
}

func TestMergeTemplates_ExtraUserFile(t *testing.T) {
	defaults := []TemplateFile{
		{Name: "IDENTITY.md", Meta: TemplateMeta{Priority: 10, ReadWhen: "always"}, Body: "id"},
	}
	overrides := []TemplateFile{
		{Name: "CUSTOM.md", Meta: TemplateMeta{Priority: 100, ReadWhen: "always"}, Body: "custom"},
	}

	merged := MergeTemplates(defaults, overrides)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged, got %d", len(merged))
	}
}

func TestFilterByPhase_Boot(t *testing.T) {
	templates := []TemplateFile{
		{Name: "IDENTITY.md", Meta: TemplateMeta{ReadWhen: "always"}},
		{Name: "BOOT.md", Meta: TemplateMeta{ReadWhen: "boot"}},
		{Name: "BOOTSTRAP.md", Meta: TemplateMeta{ReadWhen: "first_run"}},
		{Name: "HEARTBEAT.md", Meta: TemplateMeta{ReadWhen: "heartbeat"}},
	}

	// Boot without first_run
	result := FilterByPhase(templates, PhaseBoot, false)
	if len(result) != 2 { // always + boot
		t.Errorf("boot (no first_run): expected 2, got %d", len(result))
	}

	// Boot with first_run
	result = FilterByPhase(templates, PhaseBoot, true)
	if len(result) != 3 { // always + boot + first_run
		t.Errorf("boot (first_run): expected 3, got %d", len(result))
	}
}

func TestFilterByPhase_Heartbeat(t *testing.T) {
	templates := []TemplateFile{
		{Name: "IDENTITY.md", Meta: TemplateMeta{ReadWhen: "always"}},
		{Name: "BOOT.md", Meta: TemplateMeta{ReadWhen: "boot"}},
		{Name: "HEARTBEAT.md", Meta: TemplateMeta{ReadWhen: "heartbeat"}},
	}

	result := FilterByPhase(templates, PhaseHeartbeat, false)
	if len(result) != 2 { // always + heartbeat
		t.Errorf("heartbeat: expected 2, got %d", len(result))
	}
}

func TestSortByPriority(t *testing.T) {
	templates := []TemplateFile{
		{Name: "C.md", Meta: TemplateMeta{Priority: 50}},
		{Name: "A.md", Meta: TemplateMeta{Priority: 10}},
		{Name: "B.md", Meta: TemplateMeta{Priority: 30}},
	}
	SortByPriority(templates)

	if templates[0].Name != "A.md" || templates[1].Name != "B.md" || templates[2].Name != "C.md" {
		t.Errorf("sort order wrong: %v", []string{templates[0].Name, templates[1].Name, templates[2].Name})
	}
}

func TestSeedDefaults_WritesDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "templates")
	err := SeedDefaults(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all well-known defaults were written
	for _, name := range []string{"IDENTITY.md", "SOUL.md", "AGENTS.md", "USER.md", "TOOLS.md", "BOOT.md", "HEARTBEAT.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			t.Errorf("missing seeded template: %s", name)
		}
	}
}

func TestSeedDefaults_NoOpIfExists(t *testing.T) {
	dir := t.TempDir()
	// Dir already exists — write a custom file
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Custom soul"), 0644)

	err := SeedDefaults(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Custom file should be untouched
	data, _ := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if string(data) != "Custom soul" {
		t.Error("should not overwrite existing templates dir")
	}
	// No extra files should be written
	if _, err := os.Stat(filepath.Join(dir, "IDENTITY.md")); !os.IsNotExist(err) {
		t.Error("should not seed into existing dir")
	}
}

func TestSeedDefaults_EmptyDir(t *testing.T) {
	if err := SeedDefaults(""); err != nil {
		t.Errorf("expected nil for empty dir, got %v", err)
	}
}

func TestSeedDefaults_WritesManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "templates")
	if err := SeedDefaults(dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify manifest was written
	if _, err := os.Stat(filepath.Join(dir, defaultsManifest)); os.IsNotExist(err) {
		t.Error("manifest file should be created on first seed")
	}
}

func TestRefreshDefaults_UpdatesUnmodifiedFile(t *testing.T) {
	// Simulate: SeedDefaults wrote files, then binary is updated with new content.
	dir := filepath.Join(t.TempDir(), "templates")
	if err := SeedDefaults(dir); err != nil {
		t.Fatalf("seed error: %v", err)
	}

	// Read current AGENTS.md (the seeded version)
	origData, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if len(origData) == 0 {
		t.Fatal("AGENTS.md should exist after seeding")
	}

	// RefreshDefaults should be a no-op when nothing changed
	if err := RefreshDefaults(dir); err != nil {
		t.Fatalf("refresh error: %v", err)
	}
	afterData, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if string(afterData) != string(origData) {
		t.Error("RefreshDefaults should not change files when embedded content is the same")
	}
}

func TestRefreshDefaults_PreservesUserModifiedFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "templates")
	if err := SeedDefaults(dir); err != nil {
		t.Fatalf("seed error: %v", err)
	}

	// User modifies SOUL.md
	customContent := "My custom soul config"
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(customContent), 0644)

	// RefreshDefaults should NOT overwrite the user-modified file
	if err := RefreshDefaults(dir); err != nil {
		t.Fatalf("refresh error: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if string(data) != customContent {
		t.Error("RefreshDefaults should preserve user-modified files")
	}
}

func TestRefreshDefaults_NoOpWithoutMemoryDir(t *testing.T) {
	if err := RefreshDefaults(""); err != nil {
		t.Errorf("expected nil for empty dir, got %v", err)
	}
	if err := RefreshDefaults("/nonexistent/path"); err != nil {
		t.Errorf("expected nil for nonexistent dir, got %v", err)
	}
}

func TestSeedPromptFile_NoFrontmatter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	src := filepath.Join(t.TempDir(), "prompt.md")
	os.WriteFile(src, []byte("You are a helpful agent."), 0644)

	if err := SeedPromptFile(dir, src, "prompt.md"); err != nil {
		t.Fatalf("SeedPromptFile error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	content := string(data)
	if content != "---\nread_when: always\npriority: 100\n---\nYou are a helpful agent." {
		t.Errorf("unexpected content:\n%s", content)
	}
}

func TestSeedPromptFile_WithFrontmatter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	src := filepath.Join(t.TempDir(), "prompt.md")
	original := "---\npriority: 50\nread_when: boot\n---\nCustom prompt."
	os.WriteFile(src, []byte(original), 0644)

	if err := SeedPromptFile(dir, src, "prompt.md"); err != nil {
		t.Fatalf("SeedPromptFile error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if string(data) != original {
		t.Errorf("should preserve existing frontmatter, got:\n%s", string(data))
	}
}

func TestSeedPromptFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("old content"), 0644)

	src := filepath.Join(t.TempDir(), "prompt.md")
	os.WriteFile(src, []byte("new content"), 0644)

	if err := SeedPromptFile(dir, src, "prompt.md"); err != nil {
		t.Fatalf("SeedPromptFile error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if string(data) == "old content" {
		t.Error("should overwrite existing file")
	}
}

func TestSeedPromptFile_EmptyInputs(t *testing.T) {
	if err := SeedPromptFile("", "/some/path", "x.md"); err != nil {
		t.Errorf("empty memoryDir should be no-op, got: %v", err)
	}
	if err := SeedPromptFile("/some/dir", "", "x.md"); err != nil {
		t.Errorf("empty srcPath should be no-op, got: %v", err)
	}
}

func TestSeedPromptFile_MissingSrc(t *testing.T) {
	dir := t.TempDir()
	if err := SeedPromptFile(dir, "/nonexistent/file.md", "prompt.md"); err == nil {
		t.Error("expected error for missing source file")
	}
}

func TestLoadFromDir_SkipsMemoryMD(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("# Memory"), 0644)
	os.WriteFile(filepath.Join(dir, "CUSTOM.md"), []byte("# Custom"), 0644)

	files, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	for _, f := range files {
		if f.Name == "MEMORY.md" {
			t.Error("MEMORY.md should be skipped by loader")
		}
	}
	if len(files) != 1 {
		t.Errorf("expected 1 file, got %d", len(files))
	}
}
