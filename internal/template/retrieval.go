package template

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// wellKnownFiles defines the ordered list of well-known memory files and their
// display names. Files are loaded in this order if present.
var wellKnownFiles = []struct {
	filename    string
	displayName string
}{
	{"user_preferences.md", "User Preferences"},
	{"project_summary.md", "Project Summary"},
	{"decisions.md", "Decisions"},
	{"workflows.md", "Workflows"},
}

// datePrefix matches YYYY-MM-DD at the start of a filename stem.
var datePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// Retrieve loads memory sections from memoryDir in order:
// well-known files first, then extra lowercase .md files.
// Returns an empty Retrieval if memoryDir is empty or does not exist.
func Retrieve(memoryDir string) Retrieval {
	if memoryDir == "" {
		return Retrieval{}
	}

	// Build a set of well-known filenames for fast lookup.
	wellKnown := make(map[string]struct{}, len(wellKnownFiles))
	for _, wk := range wellKnownFiles {
		wellKnown[wk.filename] = struct{}{}
	}

	var files []MemoryFile

	// 1. Load well-known files in order.
	for _, wk := range wellKnownFiles {
		path := filepath.Join(memoryDir, wk.filename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // missing — skip
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		files = append(files, MemoryFile{Name: wk.displayName, Content: content})
	}

	// 2. Scan for extra lowercase .md files.
	entries, err := os.ReadDir(memoryDir)
	if err != nil {
		return Retrieval{Files: files}
	}

	// Files to skip — prompt template files and date-prefixed daily logs are
	// excluded from memory loading (they are used for other purposes).
	skipExact := map[string]struct{}{
		"agent.md":  {},
		"prompt.md": {},
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}

		// Skip if in well-known list (already handled).
		if _, ok := wellKnown[name]; ok {
			continue
		}

		// Skip template files.
		if _, ok := skipExact[name]; ok {
			continue
		}

		// Skip daily log files (YYYY-MM-DD prefix).
		stem := strings.TrimSuffix(name, ".md")
		if datePrefix.MatchString(stem) {
			continue
		}

		data, err := os.ReadFile(filepath.Join(memoryDir, name))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}

		displayName := deriveDisplayName(stem)
		files = append(files, MemoryFile{Name: displayName, Content: content})
	}

	return Retrieval{Files: files}
}

// deriveDisplayName converts a filename stem to a display name:
// strip .md (already done), replace _ and - with space, capitalize first letter.
func deriveDisplayName(stem string) string {
	s := strings.NewReplacer("_", " ", "-", " ").Replace(stem)
	if s == "" {
		return stem
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
