package proactive

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Item is one line of agenda.md: an open loop the agent left itself.
//
//   - [ ] check the v0.1.4 deploy is healthy · due: 2026-11-08T18:00Z · id: deploy-v014
//   - [x] answer Alice about the SSO quote · id: sso-quote
//
// Fields after the text are separated by " · " or " | ". due is RFC 3339,
// "2006-01-02 15:04" or "2006-01-02" in the policy's timezone.
type Item struct {
	ID   string // its id: field, else a hash of its text
	Text string
	Due  time.Time // zero: none
	Done bool
	// BadDue is a due: the parser could not read; the item is shown to
	// triage as due, so a typo does not hide it.
	BadDue string
}

// ParseAgenda reads agenda.md's checklist lines; other lines are ignored.
func ParseAgenda(content string, loc *time.Location) []Item {
	if loc == nil {
		loc = time.Local
	}
	var items []Item
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		var done bool
		switch {
		case strings.HasPrefix(line, "- [ ] "), strings.HasPrefix(line, "* [ ] "):
		case strings.HasPrefix(line, "- [x] "), strings.HasPrefix(line, "- [X] "), strings.HasPrefix(line, "* [x] "):
			done = true
		default:
			continue
		}
		it := Item{Done: done}
		fields := splitFields(line[len("- [ ] "):])
		it.Text = strings.TrimSpace(fields[0])
		for _, f := range fields[1:] {
			key, val, ok := strings.Cut(f, ":")
			if !ok {
				it.Text += " · " + strings.TrimSpace(f)
				continue
			}
			val = strings.TrimSpace(val)
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "due":
				if t, ok := parseDue(val, loc); ok {
					it.Due = t
				} else {
					it.BadDue = val
				}
			case "id":
				it.ID = val
			case "done":
				it.Done = true
			default:
				it.Text += " · " + strings.TrimSpace(f)
			}
		}
		if it.Text == "" {
			continue
		}
		if it.ID == "" {
			sum := sha256.Sum256([]byte(it.Text))
			it.ID = hex.EncodeToString(sum[:6])
		}
		items = append(items, it)
	}
	return items
}

// IsDue reports whether the item is open and its time has come (or its due
// time could not be read).
func (it Item) IsDue(now time.Time) bool {
	if it.Done {
		return false
	}
	if it.BadDue != "" {
		return true
	}
	return !it.Due.IsZero() && !now.Before(it.Due)
}

func splitFields(s string) []string {
	s = strings.ReplaceAll(s, " | ", " · ")
	return strings.Split(s, " · ")
}

func parseDue(v string, loc *time.Location) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04Z07:00", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
