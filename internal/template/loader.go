package template

import (
	"embed"
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
