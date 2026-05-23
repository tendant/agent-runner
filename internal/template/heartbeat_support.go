package template

import (
	"embed"
	"strings"
)

//go:embed defaults/HEARTBEAT.md
var defaultTemplates embed.FS

// TemplateMeta is retained for compatibility with heartbeat.go.
// Only the fields used by heartbeat parsing are populated.
type TemplateMeta struct {
	Title    string
	Summary  string
	ReadWhen string
	Priority int
}

// ParseFrontmatter splits content into metadata and body.
// Frontmatter is delimited by --- lines at the top of the file.
// Returns the parsed meta (TemplateMeta) and the body content after the closing ---.
// The meta fields are populated for heartbeat.go compatibility; heartbeat.go ignores the meta.
func ParseFrontmatter(content string) (TemplateMeta, string) {
	var meta TemplateMeta

	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return meta, content
	}

	// Find closing ---
	endIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			endIdx = i
			break
		}
	}
	if endIdx < 0 {
		return meta, content
	}

	body := strings.Join(lines[endIdx+1:], "\n")
	return meta, strings.TrimSpace(body)
}

// parseKV extracts a "key: value" pair from a line.
func parseKV(line string) (string, string, bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}
