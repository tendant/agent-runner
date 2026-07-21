package template

import (
	"embed"
	"strings"
)

//go:embed defaults/HEARTBEAT.md
var defaultTemplates embed.FS

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
