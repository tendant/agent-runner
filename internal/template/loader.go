package template

import (
	"crypto/sha256"
	"embed"
	"encoding/json"
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

	manifest := make(map[string]string)
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
		manifest[e.Name()] = fmt.Sprintf("%x", sha256.Sum256(data))
	}

	// Write manifest so RefreshDefaults can detect user modifications later
	if mdata, err := json.MarshalIndent(manifest, "", "  "); err == nil {
		os.WriteFile(filepath.Join(memoryDir, defaultsManifest), mdata, 0644)
	}
	return nil
}

const defaultsManifest = ".defaults.sha256"

// RefreshDefaults updates seeded default templates that the user has not modified.
// It tracks SHA-256 hashes of seeded files in a manifest. If a disk file matches
// the previously-seeded hash (unmodified), it is replaced with the new embedded
// version. User-modified files are left untouched.
func RefreshDefaults(memoryDir string) error {
	if memoryDir == "" {
		return nil
	}
	if _, err := os.Stat(memoryDir); os.IsNotExist(err) {
		return nil // no memory dir yet — SeedDefaults will handle it
	}

	// Load the manifest of previously-seeded hashes
	manifestPath := filepath.Join(memoryDir, defaultsManifest)
	manifest := make(map[string]string) // filename → sha256 hex
	if data, err := os.ReadFile(manifestPath); err == nil {
		json.Unmarshal(data, &manifest)
	}

	// Load current embedded defaults
	entries, err := defaultTemplates.ReadDir("defaults")
	if err != nil {
		return fmt.Errorf("read embedded defaults: %w", err)
	}

	updated := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}

		embeddedData, err := defaultTemplates.ReadFile("defaults/" + e.Name())
		if err != nil {
			continue
		}
		embeddedHash := fmt.Sprintf("%x", sha256.Sum256(embeddedData))

		diskPath := filepath.Join(memoryDir, e.Name())
		diskData, err := os.ReadFile(diskPath)
		if err != nil {
			if os.IsNotExist(err) {
				// File was deleted by user or never seeded — write it
				if writeErr := os.WriteFile(diskPath, embeddedData, 0644); writeErr == nil {
					manifest[e.Name()] = embeddedHash
					updated = true
				}
			}
			continue
		}

		diskHash := fmt.Sprintf("%x", sha256.Sum256(diskData))

		// If the disk file matches the previously-seeded hash, it's unmodified — refresh it.
		// Also refresh if there's no manifest entry (pre-manifest deployment).
		prevHash, hasPrev := manifest[e.Name()]
		if diskHash == embeddedHash {
			// Already up to date
			manifest[e.Name()] = embeddedHash
			continue
		}
		if hasPrev && diskHash == prevHash {
			// Unmodified by user — safe to update
			if writeErr := os.WriteFile(diskPath, embeddedData, 0644); writeErr == nil {
				manifest[e.Name()] = embeddedHash
				updated = true
			}
		} else if !hasPrev {
			// No manifest entry — this is a pre-manifest deployment.
			// Be conservative: only update if file matches a well-known default
			// and looks like it was never customized (has standard frontmatter title).
			// Skip to avoid overwriting user customizations.
			manifest[e.Name()] = diskHash
			updated = true
		}
		// If diskHash != prevHash and hasPrev, user modified it — leave it alone.
	}

	if updated {
		if data, err := json.MarshalIndent(manifest, "", "  "); err == nil {
			os.WriteFile(manifestPath, data, 0644)
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
//
// Deprecated: Use LoadPromptFile instead to read prompt files directly without
// copying into the memory directory.
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

// LoadPromptFile reads a prompt file from srcPath and returns it as a
// TemplateFile ready for the composition pipeline. The file is read directly
// from its source location — nothing is copied into the memory directory.
// destName is used as the template name for merging (e.g. "prompt.md").
func LoadPromptFile(srcPath, destName string) (*TemplateFile, error) {
	if srcPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("read prompt file %s: %w", srcPath, err)
	}
	content := string(data)

	// If no frontmatter, prepend defaults so parseTemplateFile picks them up
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		content = "---\nread_when: always\npriority: 100\n---\n" + content
	}

	tf := parseTemplateFile(destName, content)
	return &tf, nil
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
