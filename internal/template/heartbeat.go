package template

import (
	"os"
	"strconv"
	"strings"
)

// HeartbeatConfig holds configuration parsed from HEARTBEAT.md frontmatter.
type HeartbeatConfig struct {
	IntervalSeconds int    // interval_seconds from frontmatter (default: 300)
	Prompt          string // body content of HEARTBEAT.md
}

// ParseHeartbeatConfig extracts heartbeat configuration from a HEARTBEAT.md
// template file. Checks user overrides first, then embedded defaults.
func ParseHeartbeatConfig(templatesDir string) HeartbeatConfig {
	cfg := HeartbeatConfig{IntervalSeconds: 300}

	var content string
	if templatesDir != "" {
		if data, err := os.ReadFile(templatesDir + "/HEARTBEAT.md"); err == nil {
			content = string(data)
		}
	}
	if content == "" {
		if data, err := defaultTemplates.ReadFile("defaults/HEARTBEAT.md"); err == nil {
			content = string(data)
		}
	}
	if content == "" {
		return cfg
	}

	_, body := ParseFrontmatter(content)
	cfg.Prompt = strings.TrimSpace(body)
	cfg.IntervalSeconds = parseIntervalFromContent(content)

	return cfg
}

// parseIntervalFromContent extracts interval_seconds from frontmatter content.
func parseIntervalFromContent(content string) int {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return 300
	}

	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		key, val, ok := parseKV(line)
		if ok && key == "interval_seconds" {
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				return n
			}
		}
	}
	return 300
}
