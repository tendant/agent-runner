package template

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SeedMemory copies MEMORY.md from templatesDir into memoryDir on first run.
// First run is detected by memoryDir not existing. No-op if memoryDir already
// exists or templatesDir has no MEMORY.md.
func SeedMemory(templatesDir, memoryDir string) error {
	if memoryDir == "" {
		return nil
	}
	// If memory dir already exists, this is not the first run
	if _, err := os.Stat(memoryDir); err == nil {
		return nil
	}

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}

	// Try user override first, then embedded default
	var content []byte
	if templatesDir != "" {
		if data, err := os.ReadFile(filepath.Join(templatesDir, "MEMORY.md")); err == nil {
			content = data
		}
	}

	if content == nil {
		return nil // No MEMORY.md to seed
	}

	dst := filepath.Join(memoryDir, "MEMORY.md")
	if err := os.WriteFile(dst, content, 0644); err != nil {
		return fmt.Errorf("seed MEMORY.md: %w", err)
	}
	return nil
}

// ComposeMemorySection loads MEMORY.md and the last N daily logs from
// memoryDir. Returns empty string if no memory content is found.
func ComposeMemorySection(memoryDir string, days int) string {
	if memoryDir == "" {
		return ""
	}
	if days <= 0 {
		days = 7
	}

	var parts []string

	// Load curated MEMORY.md
	memPath := filepath.Join(memoryDir, "MEMORY.md")
	if data, err := os.ReadFile(memPath); err == nil {
		content := strings.TrimSpace(string(data))
		if content != "" {
			parts = append(parts, content)
		}
	}

	// Load daily logs
	dailyLogs := loadDailyLogs(memoryDir, days)
	if len(dailyLogs) > 0 {
		parts = append(parts, dailyLogs...)
	}

	if len(parts) == 0 {
		return ""
	}

	return "## Memory\n\n" + strings.Join(parts, "\n\n---\n\n")
}

// AppendDailyLog appends a timestamped entry to today's daily log file
// (memoryDir/YYYY-MM-DD.md). Creates the directory if needed.
func AppendDailyLog(memoryDir, entry string) error {
	if memoryDir == "" {
		return nil
	}
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	filename := time.Now().Format("2006-01-02") + ".md"
	path := filepath.Join(memoryDir, filename)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open daily log: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("write daily log: %w", err)
	}
	return nil
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
