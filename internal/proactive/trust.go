package proactive

import (
	"fmt"
	"slices"
	"strings"

	"github.com/agent-runner/agent-runner/internal/trust"
)

// TrustStatus is /trust: each class's live level, record and the
// promotion the runner suggests.
func (s *Service) TrustStatus() string {
	pol := s.policy()
	if len(pol.Classes) == 0 {
		return "No action classes: every proactive finding is at most proposed.\n\n" + trustHelp
	}
	var b strings.Builder
	b.WriteString("**Action classes** (live levels; PROACTIVE.md sets only where each starts)\n\n")
	for _, c := range pol.Classes {
		level := c.Trust
		var rec trust.Class
		if s.cfg.Trust != nil {
			level = s.cfg.Trust.Level(c.Name, c.Trust)
			rec, _ = s.cfg.Trust.Get(c.Name)
		}
		h := rec.History
		fmt.Fprintf(&b, "- **%s**: %s", c.Name, level)
		if level != c.Trust {
			fmt.Fprintf(&b, " (starts at %s)", c.Trust)
		}
		fmt.Fprintf(&b, " · proposed %d, ran %d, succeeded %d, failed %d, demoted %d", h.Proposed, h.Executed, h.Succeeded, h.Failed, h.Demoted)
		if rec.Reason != "" && level != c.Trust {
			fmt.Fprintf(&b, " · %s", rec.Reason)
		}
		b.WriteString("\n")
		if sug := rec.Suggestion(c.Name); sug != "" {
			b.WriteString("  - suggested: " + sug + "\n")
		}
	}
	if !s.cfg.Settings().Sandboxed {
		b.WriteString("\nAGENT_SANDBOX is not strict: no class runs without asking, whatever its level.\n")
	}
	return b.String() + "\n" + trustHelp
}

// SetTrust is /trust <class> <level>: the operator's, the only way up.
func (s *Service) SetTrust(class, level string) (string, error) {
	if s.cfg.Trust == nil {
		return "", fmt.Errorf("trust levels are not available in this process")
	}
	l, ok := trust.ParseLevel(level)
	if !ok {
		return "", fmt.Errorf("unknown level %q: off, propose, notify or quiet", level)
	}
	pol := s.policy()
	c, ok := pol.Class(class)
	if !ok {
		var names []string
		for _, c := range pol.Classes {
			names = append(names, c.Name)
		}
		if len(names) == 0 {
			return "", fmt.Errorf("PROACTIVE.md has no action classes (/proactive policy)")
		}
		return "", fmt.Errorf("no class %q in PROACTIVE.md (%s)", class, strings.Join(names, ", "))
	}
	s.cfg.Trust.Level(c.Name, c.Trust) // seed first, so the record keeps its history
	from := s.cfg.Trust.Set(c.Name, l, "set by the operator (/trust)")
	reply := fmt.Sprintf("ok %s: %s → %s", c.Name, from, l)
	if l.Executes() && !s.cfg.Settings().Sandboxed {
		reply += "\n\nIt still only proposes until AGENT_SANDBOX is strict."
	}
	return reply, nil
}

// classNames lists the policy's classes with their live levels, for status.
func (s *Service) classNames(pol Policy) string {
	var out []string
	for _, c := range pol.Classes {
		l := c.Trust
		if s.cfg.Trust != nil {
			l = s.cfg.Trust.Level(c.Name, c.Trust)
		}
		out = append(out, c.Name+" ("+string(l)+")")
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

const trustHelp = `  /trust                    the classes, their levels and records
  /trust <class> <level>    off | propose | notify (run, then tell) | quiet (run, log only)

A class that runs without asking drops a level by itself when a run fails, stops, scores below 6
with the reviewer, has its memory changes undone (/memory undo), has a commit it pushed reverted,
or gets a reply in its thread saying it was wrong (all within 7 days; once per run). Runs need
AGENT_SANDBOX=strict.`

// BudgetLeft is what proactive work (runs, eval replays) may still spend
// today: AGENT_PROACTIVE_BUDGET_USD, lowered by PROACTIVE.md, less what was
// spent. No budget set: unlimited.
func (s *Service) BudgetLeft() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	pol := s.policy()
	budget := cappedF(s.cfg.Settings().BudgetUSD, pol.BudgetUSD)
	if budget <= 0 {
		return 1e9
	}
	st := s.load()
	if st.Day != pol.Day(s.cfg.Now()) {
		return budget
	}
	return budget - st.Spent
}

// Spend records what proactive work outside a run spent (an eval replay)
// against today's budget.
func (s *Service) Spend(usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pol := s.policy()
	st := s.load()
	if day := pol.Day(s.cfg.Now()); st.Day != day {
		st.Day, st.Posts, st.Triages, st.Sessions, st.Spent = day, 0, 0, 0, 0
	}
	st.Spent += usd
	s.save(st)
}
