// Package proactive wakes the agent on its own (PROACTIVE_DESIGN.md): on a
// cadence it looks at the agent's due agenda items, the sessions that failed
// with nobody watching and the watched repos' signals, and when something
// needs attention asks the fast model what to do about it, then posts a note
// or a proposal in a chat. Work runs without a person's yes only for an
// action class the operator raised to notify or quiet (/trust), under a
// strict sandbox and the daily caps; a run that goes wrong lowers it again.
package proactive

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/trust"
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
	Interval    time.Duration  // 0: the environment's
	Location    *time.Location // for quiet hours and the daily caps; nil: local time
	QuietFrom   int            // minutes after midnight; QuietFrom == QuietTo: none
	QuietTo     int
	MaxPosts    int // 0: the environment's; never raises it
	MaxTriages  int
	MaxSessions int     // proactive runs per day; 0: the environment's; never raises it
	BudgetUSD   float64 // what proactive runs may spend per day; 0: the environment's; never raises it
	// Signals are the probes to run (signals: repo-ci, repo-commits), on
	// Repos (repos: a, b; empty: every registered repo).
	Signals []string
	Repos   []string
	// Classes are the action classes, from an "Action classes" section
	// of the body (§7).
	Classes []Class
	// RetroDay and RetroAt are when the weekly retrospective runs
	// (retrospective: mon 08:00); default Monday 08:00.
	RetroDay time.Weekday
	RetroAt  int    // minutes after midnight
	Text     string // the body: what the operator wants raised, proposed or ignored
	Problems []string
}

// ParsePolicy reads PROACTIVE.md. A malformed line is reported in Problems
// and otherwise ignored: a bad policy never stops the checks.
func ParsePolicy(content string) Policy {
	p := Policy{RetroDay: time.Monday, RetroAt: 8 * 60}
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
		case "retrospective":
			p.RetroDay, p.RetroAt, err = parseWeekly(val)
		case "max_sessions_per_day":
			p.MaxSessions, err = strconv.Atoi(val)
		case "budget_usd_per_day":
			p.BudgetUSD, err = strconv.ParseFloat(strings.TrimPrefix(val, "$"), 64)
		case "signals":
			for _, sig := range list(val) {
				if sig != SignalCI && sig != SignalCommits {
					p.Problems = append(p.Problems, "signals: unknown signal "+sig+" (repo-ci, repo-commits)")
					continue
				}
				p.Signals = append(p.Signals, sig)
			}
		case "repos":
			p.Repos = list(val)
		default:
			err = fmt.Errorf("unknown setting")
		}
		if err != nil {
			p.Problems = append(p.Problems, key+": "+err.Error())
		}
	}
	p.Classes, p.Problems = parseClasses(p.Text, p.Problems)
	return p
}

// Signal probes (§5.2).
const (
	SignalCI      = "repo-ci"      // the latest CI result on a repo's branch (GitHub)
	SignalCommits = "repo-commits" // new commits on a repo's branch
)

// Class is an action class: a named kind of work the operator lets the
// agent do proactively, at an initial trust level.
type Class struct {
	Name        string
	Description string
	Trust       trust.Level // the initial level; the live one is in trust.json
	IgnoreQuiet bool        // quiet_hours: ignore
}

// Class finds a class by name.
func (p Policy) Class(name string) (Class, bool) {
	for _, c := range p.Classes {
		if strings.EqualFold(c.Name, strings.TrimSpace(name)) {
			return c, true
		}
	}
	return Class{}, false
}

var classLine = regexp.MustCompile(`^[-*]\s+([a-z0-9][a-z0-9-]*)\s*:\s*(.*)$`)

// parseClasses reads the classes from the body's "Action classes" section:
//
//	# Action classes
//
//	- ci-fix: CI failing on a watched repo's default branch. Investigate, fix, push.
//	  trust: propose
//	- daily-digest: a morning summary of what changed. quiet_hours: ignore
//	  trust: notify
//
// A class is a list item; trust: and quiet_hours: may follow on its line or
// on the indented lines below it.
func parseClasses(body string, problems []string) ([]Class, []string) {
	var out []Class
	in := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			in = strings.Contains(strings.ToLower(line), "action classes")
			continue
		}
		if !in || line == "" {
			continue
		}
		if m := classLine.FindStringSubmatch(line); m != nil && raw == strings.TrimLeft(raw, " \t") {
			out = append(out, Class{Name: m[1], Trust: trust.Propose})
			line = m[2]
		} else if len(out) == 0 {
			continue
		}
		c := &out[len(out)-1]
		var desc []string
		for _, part := range strings.Split(line, ". ") {
			k, v, ok := strings.Cut(part, ":")
			k = strings.ToLower(strings.TrimSpace(k))
			v = strings.TrimSuffix(strings.TrimSpace(v), ".")
			switch {
			case ok && k == "trust":
				l, good := trust.ParseLevel(v)
				if !good {
					problems = append(problems, c.Name+": unknown trust level "+v+" (off, propose, notify, quiet)")
					continue
				}
				c.Trust = l
			case ok && k == "quiet_hours":
				c.IgnoreQuiet = strings.EqualFold(v, "ignore")
			default:
				desc = append(desc, strings.TrimSpace(part))
			}
		}
		if d := strings.TrimSpace(strings.Join(desc, ". ")); d != "" {
			if c.Description != "" {
				c.Description += " "
			}
			c.Description += d
		}
	}
	return out, problems
}

// list splits "a, b c" into its items.
func list(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.Trim(v, "[]"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.Trim(f, `"'`); f != "" {
			out = append(out, f)
		}
	}
	return out
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

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseWeekly reads "mon 08:00" (a weekday, by its first three letters or
// in full, then HH:MM).
func parseWeekly(v string) (time.Weekday, int, error) {
	day, at, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok {
		return time.Monday, 8 * 60, fmt.Errorf("want a weekday and HH:MM, e.g. mon 08:00")
	}
	day = strings.ToLower(day)
	if len(day) > 3 {
		day = day[:3]
	}
	wd, ok := weekdays[day]
	if !ok {
		return time.Monday, 8 * 60, fmt.Errorf("unknown weekday %q", day)
	}
	m, err := clock(at)
	if err != nil {
		return time.Monday, 8 * 60, err
	}
	return wd, m, nil
}

// NextRetro is the first retrospective slot after t.
func (p Policy) NextRetro(after time.Time) time.Time {
	t := p.in(after)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	for i := 0; i <= 7; i++ {
		d := day.AddDate(0, 0, i)
		if d.Weekday() != p.RetroDay {
			continue
		}
		slot := time.Date(d.Year(), d.Month(), d.Day(), p.RetroAt/60, p.RetroAt%60, 0, 0, d.Location())
		if slot.After(after) {
			return slot
		}
	}
	return day.AddDate(0, 0, 14) // unreachable
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

func (p Policy) anyIgnoresQuiet() bool {
	for _, c := range p.Classes {
		if c.IgnoreQuiet {
			return true
		}
	}
	return false
}
