// Package proactive wakes the agent on its own (PROACTIVE_DESIGN.md): on a
// cadence it looks at the agent's due agenda items and the sessions that
// failed with nobody watching, and when something needs attention asks the
// fast model what to do about it, then posts a note or a proposal in a chat.
// Phase 1 never runs work by itself: a proposal waits for a person's yes,
// through the ordinary confirmation flow of the thread it opens.
package proactive

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Files in the memory dir. Upper case, so a session cannot edit them
// (template/stage.go): both are the operator's.
const (
	PolicyFile = "PROACTIVE.md" // cadence, quiet hours, caps, and what to raise or ignore
	GoalsFile  = "GOALS.md"     // standing objectives, in every prompt and in triage
	AgendaFile = "agenda.md"    // the agent's open loops, written through the staged merge
)

// Policy is PROACTIVE.md: front matter for the runner, the body for triage.
//
//	---
//	interval: 1h
//	timezone: America/Los_Angeles
//	quiet_hours: 22:00-07:00
//	max_posts_per_day: 5
//	max_triages_per_day: 12
//	---
//	Failed nightly jobs: propose a fix. Agenda items about deploys: raise them...
type Policy struct {
	Interval   time.Duration  // 0: the environment's
	Location   *time.Location // for quiet hours and the daily caps; nil: local time
	QuietFrom  int            // minutes after midnight; QuietFrom == QuietTo: none
	QuietTo    int
	MaxPosts   int // 0: the environment's; never raises it
	MaxTriages int
	Text       string // the body: what the operator wants raised, proposed or ignored
	Problems   []string
}

// ParsePolicy reads PROACTIVE.md. A malformed line is reported in Problems
// and otherwise ignored: a bad policy never stops the checks.
func ParsePolicy(content string) Policy {
	var p Policy
	front, body := splitFrontMatter(content)
	p.Text = strings.TrimSpace(body)
	for _, line := range strings.Split(front, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			if strings.TrimSpace(line) != "" {
				p.Problems = append(p.Problems, "not key: value: "+strings.TrimSpace(line))
			}
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		var err error
		switch key {
		case "interval":
			p.Interval, err = time.ParseDuration(val)
		case "timezone":
			p.Location, err = time.LoadLocation(val)
		case "quiet_hours":
			p.QuietFrom, p.QuietTo, err = parseQuietHours(val)
		case "max_posts_per_day":
			p.MaxPosts, err = strconv.Atoi(val)
		case "max_triages_per_day":
			p.MaxTriages, err = strconv.Atoi(val)
		default:
			err = fmt.Errorf("unknown setting")
		}
		if err != nil {
			p.Problems = append(p.Problems, key+": "+err.Error())
		}
	}
	return p
}

// Quiet reports whether t falls in the policy's quiet hours.
func (p Policy) Quiet(t time.Time) bool {
	if p.QuietFrom == p.QuietTo {
		return false
	}
	t = p.in(t)
	m := t.Hour()*60 + t.Minute()
	if p.QuietFrom < p.QuietTo {
		return m >= p.QuietFrom && m < p.QuietTo
	}
	return m >= p.QuietFrom || m < p.QuietTo // over midnight
}

// Day names t's day in the policy's timezone, for the daily caps.
func (p Policy) Day(t time.Time) string { return p.in(t).Format("2006-01-02") }

func (p Policy) in(t time.Time) time.Time {
	if p.Location != nil {
		return t.In(p.Location)
	}
	return t.Local()
}

func parseQuietHours(v string) (from, to int, err error) {
	a, b, ok := strings.Cut(v, "-")
	if !ok {
		return 0, 0, fmt.Errorf("want HH:MM-HH:MM")
	}
	if from, err = clock(a); err != nil {
		return 0, 0, err
	}
	if to, err = clock(b); err != nil {
		return 0, 0, err
	}
	return from, to, nil
}

func clock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("want HH:MM, got %q", strings.TrimSpace(s))
	}
	return t.Hour()*60 + t.Minute(), nil
}

// splitFrontMatter separates a leading "---" block from the rest.
func splitFrontMatter(content string) (front, body string) {
	rest, ok := strings.CutPrefix(strings.TrimLeft(content, "\ufeff"), "---\n")
	if !ok {
		return "", content
	}
	if i := strings.Index(rest, "\n---"); i >= 0 {
		after := rest[i+len("\n---"):]
		if nl := strings.IndexByte(after, '\n'); nl >= 0 {
			after = after[nl+1:]
		} else {
			after = ""
		}
		return rest[:i], after
	}
	return "", content
}
