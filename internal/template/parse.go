package template

import (
	"strconv"
	"strings"
)

// ParseFrontmatter splits a template into metadata and body.
// Frontmatter is delimited by --- lines at the top of the file.
// Returns the parsed meta and the body content after the closing ---.
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

	// Parse key: value pairs from frontmatter
	for _, line := range lines[1:endIdx] {
		key, val, ok := parseKV(line)
		if !ok {
			continue
		}
		switch key {
		case "title":
			meta.Title = val
		case "summary":
			meta.Summary = val
		case "read_when":
			meta.ReadWhen = val
		case "priority":
			if n, err := strconv.Atoi(val); err == nil {
				meta.Priority = n
			}
		}
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
