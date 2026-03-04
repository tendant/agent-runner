package template

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ComposeMemorySection loads MEMORY.md and the last N daily logs from the
// memory/ subdirectory within templatesDir. Returns empty string if no
// memory content is found.
func ComposeMemorySection(templatesDir string, days int) string {
	if templatesDir == "" {
		return ""
	}
	if days <= 0 {
		days = 7
	}

	var parts []string

	// Load curated MEMORY.md
	memPath := filepath.Join(templatesDir, "MEMORY.md")
	if data, err := os.ReadFile(memPath); err == nil {
		content := strings.TrimSpace(string(data))
		if content != "" {
			parts = append(parts, content)
		}
	}

	// Load daily logs from memory/ subdirectory
	memDir := filepath.Join(templatesDir, "memory")
	dailyLogs := loadDailyLogs(memDir, days)
	if len(dailyLogs) > 0 {
		parts = append(parts, dailyLogs...)
	}

	if len(parts) == 0 {
		return ""
	}

	return "## Memory\n\n" + strings.Join(parts, "\n\n---\n\n")
}

// loadDailyLogs reads the last N daily log files (YYYY-MM-DD.md) from dir,
// sorted chronologically (newest last).
func loadDailyLogs(dir string, days int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	// Filter to valid date-named .md files
	type dated struct {
		date    time.Time
		content string
	}
	var logs []dated

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		t, err := time.Parse("2006-01-02", name)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content != "" {
			logs = append(logs, dated{date: t, content: content})
		}
	}

	// Sort by date ascending
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].date.Before(logs[j].date)
	})

	// Take last N
	if len(logs) > days {
		logs = logs[len(logs)-days:]
	}

	result := make([]string, len(logs))
	for i, l := range logs {
		result[i] = fmt.Sprintf("### %s\n\n%s", l.date.Format("2006-01-02"), l.content)
	}
	return result
}
