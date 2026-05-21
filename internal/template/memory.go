package template

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ComposeMemorySection loads MEMORY.md and the last N daily logs from
// memoryDir. Returns empty string if no memory content is found.
// charCap limits the total characters returned (0 = no limit); oldest
// daily logs are dropped first when the cap is exceeded.
func ComposeMemorySection(memoryDir string, days int, charCap int) string {
	if memoryDir == "" {
		return ""
	}
	if days <= 0 {
		days = 7
	}

	var memContent string
	// Load curated MEMORY.md
	memPath := filepath.Join(memoryDir, "MEMORY.md")
	if data, err := os.ReadFile(memPath); err == nil {
		content := strings.TrimSpace(string(data))
		if content != "" {
			memContent = content
		}
	}

	// Load daily logs
	dailyLogs := loadDailyLogs(memoryDir, days)

	// Apply char cap: drop oldest logs until we fit
	if charCap > 0 {
		for len(dailyLogs) > 0 {
			total := len(memContent)
			for _, l := range dailyLogs {
				total += len(l)
			}
			if total <= charCap {
				break
			}
			dailyLogs = dailyLogs[1:] // drop oldest
		}
		// Truncate MEMORY.md if it alone exceeds the cap
		if len(memContent) > charCap {
			memContent = memContent[:charCap] + "\n... (truncated)"
		}
	}

	var parts []string
	if memContent != "" {
		parts = append(parts, memContent)
	}
	if len(dailyLogs) > 0 {
		parts = append(parts, dailyLogs...)
	}

	if len(parts) == 0 {
		return ""
	}

	return "## Memory\n\n" + strings.Join(parts, "\n\n---\n\n")
}

// AppendDailyLog appends a timestamped entry to today's daily log file
// (memoryDir/YYYY-MM-DD-<hostname>.md). Creates the directory if needed.
func AppendDailyLog(memoryDir, entry string) error {
	if memoryDir == "" {
		return nil
	}
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	filename := time.Now().Format("2006-01-02") + "-" + sanitizeForFilename(hostname) + ".md"
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

// sanitizeForFilename replaces characters unsafe in filenames with hyphens.
func sanitizeForFilename(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	result := b.String()
	if result == "" {
		return "local"
	}
	return result
}

// loadDailyLogs reads the last N daily log files (YYYY-MM-DD[-<suffix>].md) from
// dir, sorted chronologically (newest last). The hostname suffix is optional;
// only the first 10 characters of the filename stem are parsed as a date.
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
		// Need at least 10 chars for YYYY-MM-DD
		if len(name) < 10 {
			continue
		}
		// If longer, the 11th char must be '-' (hostname separator)
		if len(name) > 10 && name[10] != '-' {
			continue
		}
		t, err := time.Parse("2006-01-02", name[:10])
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
