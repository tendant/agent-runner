package template

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// maxRecentEntries bounds Recent Sessions: the latest few sessions, not
// every one in the window.
const maxRecentEntries = 10

// ThreadLine marks a daily-log entry with the thread (task or chat) its
// session belonged to, so a later session in the same thread sees it first.
const ThreadLine = "**Thread:** "

// RecentSessions is RecentSessionsFor with no thread.
func RecentSessions(memoryDir string, days, budget int) string {
	return RecentSessionsFor(memoryDir, days, budget, "")
}

// RecentSessionsFor collects daily-log entries (memoryDir/YYYY-MM-DD-<host>.md,
// as written by AppendDailyLog) from the last `days` days: at most
// maxRecentEntries, those of thread first (when given), then the newest
// others. They are shown under "### <date> (<host>)" headers, newest first.
// When budget > 0, entries that would exceed it are left out (the first
// one is truncated rather than dropped). Returns "" when days <= 0,
// memoryDir is empty, or no logs fall in the window.
func RecentSessionsFor(memoryDir string, days, budget int, thread string) string {
	if memoryDir == "" || days <= 0 {
		return ""
	}
	entries, err := os.ReadDir(memoryDir)
	if err != nil {
		return ""
	}
	cutoff := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")

	type entry struct {
		date, host, text string
		order            int // newest first
	}
	var all []entry
	type dayFile struct{ date, host, name string }
	var days_ []dayFile
	for _, e := range entries {
		name := e.Name()
		stem := strings.TrimSuffix(name, ".md")
		if e.IsDir() || !strings.HasSuffix(name, ".md") || !datePrefix.MatchString(stem) || stem[:10] < cutoff {
			continue
		}
		days_ = append(days_, dayFile{stem[:10], strings.TrimPrefix(strings.TrimPrefix(stem, stem[:10]), "-"), name})
	}
	// Newest day first; stable so same-day multi-host files keep dir order.
	sort.SliceStable(days_, func(i, j int) bool { return days_[i].date > days_[j].date })
	for _, d := range days_ {
		data, err := ReadMemoryFile(memoryDir, d.name)
		if err != nil {
			continue
		}
		parts := splitLogEntries(data)
		for i := len(parts) - 1; i >= 0; i-- { // appended in order: newest last
			all = append(all, entry{date: d.date, host: d.host, text: parts[i], order: len(all)})
		}
	}
	if len(all) == 0 {
		return ""
	}

	// Choose: the thread's entries first, then the newest others.
	var chosen []entry
	taken := make([]bool, len(all))
	if thread != "" {
		for i, e := range all {
			if len(chosen) < maxRecentEntries && strings.Contains(e.text, ThreadLine+thread) {
				chosen, taken[i] = append(chosen, e), true
			}
		}
	}
	for i, e := range all {
		if len(chosen) < maxRecentEntries && !taken[i] {
			chosen = append(chosen, e)
		}
	}
	if budget > 0 {
		var fit []entry
		total := 0
		for _, e := range chosen {
			if total+len(e.text) > budget {
				if len(fit) == 0 {
					e.text = truncate(e.text, budget) + fmt.Sprintf(truncationNotice, e.date+"-"+e.host+".md")
					fit = append(fit, e)
				}
				continue
			}
			fit, total = append(fit, e), total+len(e.text)
		}
		chosen = fit
	}

	// Show newest first, grouped by day file.
	sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].order < chosen[j].order })
	var b strings.Builder
	last := ""
	for _, e := range chosen {
		if key := e.date + "\x00" + e.host; key != last {
			if last != "" {
				b.WriteString("\n")
			}
			header := "### " + e.date
			if e.host != "" {
				header += " (" + e.host + ")"
			}
			b.WriteString(header + "\n\n")
			last = key
		}
		b.WriteString(e.text + "\n")
	}
	return strings.TrimSpace(b.String())
}

// splitLogEntries splits a daily log into its entries: each starts with a
// "**[HH:MM]**" line (AppendDailyLog's format); text before the first is
// one entry of its own.
func splitLogEntries(data string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if strings.HasPrefix(line, "**[") && cur.Len() > 0 {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
		cur.WriteString(line + "\n")
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}
