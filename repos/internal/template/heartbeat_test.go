package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHeartbeatConfig_Defaults(t *testing.T) {
	cfg := ParseHeartbeatConfig("")
	if cfg.IntervalSeconds != 300 {
		t.Errorf("interval = %d, want 300", cfg.IntervalSeconds)
	}
	if cfg.Prompt == "" {
		t.Error("should have default prompt from embedded template")
	}
}

func TestParseHeartbeatConfig_UserOverride(t *testing.T) {
	dir := t.TempDir()
	content := `---
title: Custom Heartbeat
read_when: heartbeat
priority: 90
interval_seconds: 120
---

Give a short status update.`
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte(content), 0644)

	cfg := ParseHeartbeatConfig(dir)
	if cfg.IntervalSeconds != 120 {
		t.Errorf("interval = %d, want 120", cfg.IntervalSeconds)
	}
	if !strings.Contains(cfg.Prompt, "short status update") {
		t.Errorf("prompt = %q", cfg.Prompt)
	}
}

func TestParseIntervalFromContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"no frontmatter", "# Just text", 300},
		{"with interval", "---\ninterval_seconds: 60\n---\nbody", 60},
		{"invalid interval", "---\ninterval_seconds: abc\n---\nbody", 300},
		{"zero interval", "---\ninterval_seconds: 0\n---\nbody", 300},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIntervalFromContent(tt.content)
			if got != tt.want {
				t.Errorf("parseIntervalFromContent() = %d, want %d", got, tt.want)
			}
		})
	}
}
