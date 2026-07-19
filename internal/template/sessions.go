package template

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// RecentSessions collects daily-log entries (memoryDir/YYYY-MM-DD-<host>.md,
// as written by AppendDailyLog) from the last `days` days and formats them as
// "### <date> (<host>)" blocks, newest first. When budget > 0 and the result
// would exceed it, whole days are dropped oldest-first. Returns "" when
// days <= 0, memoryDir is empty, or no logs fall in the window.
func RecentSessions(memoryDir string, days, budget int) string {
	if memoryDir == "" || days <= 0 {
		return ""
	}
	entries, err := os.ReadDir(memoryDir)
	if err != nil {
		return ""
	}

	cutoff := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")

	type dayLog struct {
		date    string
		host    string
		content string
	}
	var logs []dayLog
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		stem := strings.TrimSuffix(name, ".md")
		if !datePrefix.MatchString(stem) {
			continue
		}
		date := stem[:10]
		if date < cutoff {
			continue
		}
		data, err := ReadMemoryFile(memoryDir, name)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(data)
		if content == "" {
			continue
		}
		host := strings.TrimPrefix(stem, date)
		host = strings.TrimPrefix(host, "-")
		logs = append(logs, dayLog{date: date, host: host, content: content})
	}
	if len(logs) == 0 {
		return ""
	}

	// Newest first; stable so same-day multi-host files keep dir order.
	sort.SliceStable(logs, func(i, j int) bool { return logs[i].date > logs[j].date })

	var blocks []string
	total := 0
	for _, l := range logs {
		header := "### " + l.date
		if l.host != "" {
			header += " (" + l.host + ")"
		}
		block := header + "\n\n" + l.content
		if budget > 0 && total+len(block) > budget {
			if len(blocks) == 0 {
				// Even the newest day alone exceeds the budget — include a
				// truncated head rather than nothing.
				blocks = append(blocks, truncate(block, budget)+
					fmt.Sprintf(truncationNotice, l.date+"-"+l.host+".md"))
			}
			break
		}
		blocks = append(blocks, block)
		total += len(block)
	}

	return strings.Join(blocks, "\n\n")
}
