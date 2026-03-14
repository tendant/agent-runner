package template

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed defaults/*.md
var defaultTemplates embed.FS

// LoadDefaults loads all embedded default templates.
func LoadDefaults() ([]TemplateFile, error) {
	entries, err := defaultTemplates.ReadDir("defaults")
	if err != nil {
		return nil, err
	}

	var files []TemplateFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := defaultTemplates.ReadFile("defaults/" + e.Name())
		if err != nil {
			continue
		}
		tf := parseTemplateFile(e.Name(), string(data))
		files = append(files, tf)
	}
	return files, nil
}

// SeedDefaults writes all embedded default templates into memoryDir on first
// run (detected by the directory not existing). No-op if the dir already exists.
func SeedDefaults(memoryDir string) error {
	if memoryDir == "" {
		return nil
	}
	// If memory dir already exists, not first run
	if _, err := os.Stat(memoryDir); err == nil {
		return nil
	}

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}

	entries, err := defaultTemplates.ReadDir("defaults")
	if err != nil {
		return fmt.Errorf("read embedded defaults: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := defaultTemplates.ReadFile("defaults/" + e.Name())
		if err != nil {
			continue
		}
		dst := filepath.Join(memoryDir, e.Name())
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", e.Name(), err)
		}
	}
	return nil
}

// LoadFromDir loads template files from a user directory.
// Returns nil, nil if dir is empty or doesn't exist.
func LoadFromDir(dir string) ([]TemplateFile, error) {
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []TemplateFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		// Skip MEMORY.md — handled by memory system
		if e.Name() == "MEMORY.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		tf := parseTemplateFile(e.Name(), string(data))
		files = append(files, tf)
	}
	return files, nil
}

// MergeTemplates merges defaults with user overrides. User files override
// defaults by filename match. Extra user files are included with defaults
// (priority 100, read_when "always") if they have no frontmatter.
func MergeTemplates(defaults, overrides []TemplateFile) []TemplateFile {
	byName := make(map[string]TemplateFile, len(defaults))
	for _, f := range defaults {
		byName[f.Name] = f
	}
	for _, f := range overrides {
		byName[f.Name] = f
	}

	result := make([]TemplateFile, 0, len(byName))
	for _, f := range byName {
		result = append(result, f)
	}
	return result
}

// FilterByPhase returns templates matching the given phase.
// "always" templates are always included. Boot phase includes "boot" + "first_run" (if firstRun).
// Heartbeat phase includes "heartbeat" templates.
func FilterByPhase(templates []TemplateFile, phase Phase, firstRun bool) []TemplateFile {
	var result []TemplateFile
	for _, t := range templates {
		rw := t.Meta.ReadWhen
		switch {
		case rw == "always":
			result = append(result, t)
		case phase == PhaseBoot && rw == "boot":
			result = append(result, t)
		case phase == PhaseBoot && rw == "first_run" && firstRun:
			result = append(result, t)
		case phase == PhaseHeartbeat && rw == "heartbeat":
			result = append(result, t)
		}
	}
	return result
}

// SortByPriority sorts templates by priority (ascending).
func SortByPriority(templates []TemplateFile) {
	sort.Slice(templates, func(i, j int) bool {
		return templates[i].Meta.Priority < templates[j].Meta.Priority
	})
}

// SeedPromptFile copies srcPath into memoryDir/destName so it enters the
// template pipeline. If the source file already has frontmatter it is copied
// as-is; otherwise default frontmatter (read_when: always, priority: 100) is
// prepended. The source file is authoritative — the destination is always
// overwritten.
func SeedPromptFile(memoryDir, srcPath, destName string) error {
	if memoryDir == "" || srcPath == "" {
		return nil
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read prompt file %s: %w", srcPath, err)
	}
	content := string(data)

	// If no frontmatter, prepend defaults
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		content = "---\nread_when: always\npriority: 100\n---\n" + content
	}

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	dst := filepath.Join(memoryDir, destName)
	if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
		return fmt.Errorf("write %s: %w", destName, err)
	}
	return nil
}

// parseTemplateFile parses a template file from name and raw content.
// Applies well-known defaults if frontmatter fields are missing.
func parseTemplateFile(name, content string) TemplateFile {
	meta, body := ParseFrontmatter(content)

	// Apply well-known defaults for missing fields
	if wk, ok := wellKnownDefaults[name]; ok {
		if meta.Priority == 0 {
			meta.Priority = wk.Priority
		}
		if meta.ReadWhen == "" {
			meta.ReadWhen = wk.ReadWhen
		}
	} else {
		// Unknown files: default priority 100, read_when always
		if meta.Priority == 0 {
			meta.Priority = 100
		}
		if meta.ReadWhen == "" {
			meta.ReadWhen = "always"
		}
	}

	return TemplateFile{
		Name: name,
		Meta: meta,
		Body: body,
	}
}
