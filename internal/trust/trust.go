// Package trust keeps the live trust level of each proactive action class
// (PROACTIVE_DESIGN.md §12): off → propose → notify → quiet. Promotion is
// the operator's only (/trust); the runner demotes a class one level on its
// own when one of its runs goes wrong. Levels live in STATE_ROOT/trust.json,
// apart from PROACTIVE.md, so a demotion never rewrites the operator's file.
package trust

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level is how much a class may do without asking.
type Level string

const (
	Off     Level = "off"     // never proposed or run
	Propose Level = "propose" // proposed; runs on a person's yes
	Notify  Level = "notify"  // runs, then tells the chat
	Quiet   Level = "quiet"   // runs, and is only logged
)

var order = []Level{Off, Propose, Notify, Quiet}

// ParseLevel reads a level's name.
func ParseLevel(s string) (Level, bool) {
	l := Level(strings.ToLower(strings.TrimSpace(s)))
	for _, o := range order {
		if l == o {
			return l, true
		}
	}
	return "", false
}

func (l Level) rank() int {
	for i, o := range order {
		if l == o {
			return i
		}
	}
	return 0
}

// Down is one level lower (off stays off).
func (l Level) Down() Level { return order[max(0, l.rank()-1)] }

// Executes reports whether a class at l runs without asking.
func (l Level) Executes() bool { return l == Notify || l == Quiet }

// PromoteAfter is how many clean runs in a row at a level make the runner
// suggest the next one.
const PromoteAfter = 10

// History counts what a class's proactive work came to.
type History struct {
	Proposed  int `json:"proposed,omitempty"`
	Executed  int `json:"executed,omitempty"`
	Succeeded int `json:"succeeded,omitempty"`
	Failed    int `json:"failed,omitempty"`
	Demoted   int `json:"demoted,omitempty"`
}

// Class is one class's live level and record.
type Class struct {
	Level   Level     `json:"level"`
	Clean   int       `json:"clean"` // clean runs in a row at this level
	History History   `json:"history"`
	Changed time.Time `json:"changed,omitzero"`
	Reason  string    `json:"reason,omitempty"` // why it has its level
}

// Store is trust.json. Safe for concurrent use in one process.
type Store struct {
	path string
	mu   sync.Mutex
	Now  func() time.Time
}

// New returns the store at path.
func New(path string) *Store { return &Store{path: path, Now: time.Now} }

// Level is a class's live level, seeded from initial (PROACTIVE.md's
// trust:) the first time the class is seen.
func (s *Store) Level(name string, initial Level) Level {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.read()
	if c, ok := m[name]; ok {
		return c.Level
	}
	if initial == "" {
		initial = Propose
	}
	m[name] = Class{Level: initial, Changed: s.Now().UTC(), Reason: "initial level from PROACTIVE.md"}
	s.write(m)
	return initial
}

// Set is the operator's /trust <class> <level>.
func (s *Store) Set(name string, l Level, reason string) (from Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.read()
	c := m[name]
	from = c.Level
	if c.Level != l {
		c.Clean = 0
	}
	c.Level, c.Changed, c.Reason = l, s.Now().UTC(), reason
	m[name] = c
	s.write(m)
	return from
}

// Proposed records a proposal of the class's work (a plan posted).
func (s *Store) Proposed(name string) { s.update(name, func(c *Class) { c.History.Proposed++ }) }

// Executed records a run started without asking.
func (s *Store) Executed(name string) { s.update(name, func(c *Class) { c.History.Executed++ }) }

// Succeeded records a clean run.
func (s *Store) Succeeded(name string) {
	s.update(name, func(c *Class) { c.History.Succeeded++; c.Clean++ })
}

// Failed records a run that went wrong and demotes the class one level.
func (s *Store) Failed(name, reason string) (from, to Level) {
	s.update(name, func(c *Class) { c.History.Failed++ })
	return s.Demote(name, reason)
}

// Demote lowers a class one level (an undone run, a failure).
func (s *Store) Demote(name, reason string) (from, to Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.read()
	c, ok := m[name]
	if !ok {
		c.Level = Propose
	}
	from, to = c.Level, c.Level.Down()
	if to != from {
		c.History.Demoted++
	}
	c.Level, c.Clean, c.Changed, c.Reason = to, 0, s.Now().UTC(), reason
	m[name] = c
	s.write(m)
	slog.Info("trust: class demoted", "class", name, "from", from, "to", to, "reason", reason)
	return from, to
}

// Get is a class's record, if it has one.
func (s *Store) Get(name string) (Class, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.read()[name]
	return c, ok
}

// Suggestion is the promotion the runner suggests for a class, or "".
func (c Class) Suggestion(name string) string {
	if c.Clean < PromoteAfter || c.Level == Quiet || c.Level == Off {
		return ""
	}
	next := order[c.Level.rank()+1]
	return fmt.Sprintf("%d clean runs in a row at %s: /trust %s %s to promote it", c.Clean, c.Level, name, next)
}

// Names is every class with a record, sorted.
func (s *Store) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.read() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Store) update(name string, fn func(*Class)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.read()
	c, ok := m[name]
	if !ok {
		c.Level = Propose
	}
	fn(&c)
	m[name] = c
	s.write(m)
}

func (s *Store) read() map[string]Class {
	m := map[string]Class{}
	data, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(data, &m); err != nil {
			slog.Warn("trust: trust.json unreadable; every class starts again from PROACTIVE.md", "error", err)
			m = map[string]Class{}
		}
	}
	return m
}

func (s *Store) write(m map[string]Class) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		slog.Warn("trust: could not save", "error", err)
		return
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		err = os.Rename(tmp, s.path)
		if err != nil {
			slog.Warn("trust: could not save", "error", err)
		}
	}
}
