package evals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/subagent"
)

// Runner replays the eval set for a comparison. One comparison at a time.
type Runner struct {
	MemoryDir string
	// Start starts an eval replay (execution.Engine.StartEval); Get finds
	// a session by id.
	Start func(message string, spec *agent.EvalSpec) (string, error)
	Get   func(id string) (*agent.Session, bool)
	// Ready says why replays cannot run now (no sandbox), or nil.
	Ready func() error
	// BudgetLeft is what proactive work may still spend today
	// (AGENT_PROACTIVE_BUDGET_USD), and Spend records a replay's cost
	// against it. nil: no budget.
	BudgetLeft func() float64
	Spend      func(usd float64)
	Poll       time.Duration // default 5s

	mu      sync.Mutex
	running bool
}

// Variants of a comparison.
const (
	Current  = "current"
	Proposed = "proposed"
)

// Run is one replay's outcome.
type Run struct {
	Eval      string
	Variant   string
	SessionID string
	Status    string
	Passed    int
	Total     int
	Score     int // the reviewer's, 0: none
	CostUSD   float64
	Failed    []string // the checks that did not pass, and why
	Reply     string   // the agent's last reply, to see why it did what it did
}

// Report is a comparison's outcome.
type Report struct {
	Runs    []Run
	CostUSD float64
	Stopped string // why it stopped early, "" when it ran the whole set
}

// Check says whether a comparison could start now, and how many evals it
// would replay.
func (r *Runner) Check() (int, error) {
	if r.Ready != nil {
		if err := r.Ready(); err != nil {
			return 0, err
		}
	}
	set, err := r.usable()
	if err != nil {
		return 0, err
	}
	if r.BudgetLeft != nil && r.BudgetLeft() <= 0 {
		return 0, errors.New("today's AGENT_PROACTIVE_BUDGET_USD is spent")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return 0, errors.New("a comparison is already running")
	}
	return len(set), nil
}

func (r *Runner) usable() ([]Eval, error) {
	all, err := List(r.MemoryDir)
	if err != nil {
		return nil, err
	}
	var set []Eval
	for _, e := range all {
		if e.Usable() {
			set = append(set, e)
		}
	}
	if len(set) == 0 {
		return nil, errors.New("there are no evals to replay (/evals set <name> ...)")
	}
	return set[:min(len(set), MaxEvals)], nil
}

// Compare replays each eval with the current instructions and with
// overrides, one replay at a time, and reports what passed. It stops early
// when the budget runs out or ctx ends.
func (r *Runner) Compare(ctx context.Context, overrides map[string]string) (Report, error) {
	if _, err := r.Check(); err != nil {
		return Report{}, err
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return Report{}, errors.New("a comparison is already running")
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	set, err := r.usable()
	if err != nil {
		return Report{}, err
	}
	var rep Report
	for _, e := range set {
		for _, variant := range []string{Current, Proposed} {
			if r.BudgetLeft != nil && r.BudgetLeft() <= 0 {
				rep.Stopped = "today's AGENT_PROACTIVE_BUDGET_USD is spent"
				return rep, nil
			}
			if ctx.Err() != nil {
				rep.Stopped = "stopped"
				return rep, nil
			}
			spec := &agent.EvalSpec{Checks: e.Checks}
			if variant == Proposed {
				spec.Overrides = overrides
			}
			run := r.replay(ctx, e, variant, spec)
			rep.Runs = append(rep.Runs, run)
			rep.CostUSD += run.CostUSD
			if r.Spend != nil && run.CostUSD > 0 {
				r.Spend(run.CostUSD)
			}
		}
	}
	return rep, nil
}

func (r *Runner) replay(ctx context.Context, e Eval, variant string, spec *agent.EvalSpec) Run {
	run := Run{Eval: e.Name, Variant: variant, Total: len(e.Checks)}
	id, err := r.Start(e.Message, spec)
	if err != nil {
		run.Status, run.Failed = "not started", []string{err.Error()}
		return run
	}
	run.SessionID = id
	poll := r.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	for {
		s, ok := r.Get(id)
		if !ok {
			run.Status = "lost"
			return run
		}
		switch s.Status {
		case agent.SessionStatusCompleted, agent.SessionStatusFailed, agent.SessionStatusStopped:
			run.Status = string(s.Status)
			for i := len(s.Iterations) - 1; i >= 0; i-- {
				if out := strings.TrimSpace(s.Iterations[i].Output); out != "" {
					run.Reply = out
					break
				}
			}
			run.CostUSD = s.TotalCostUSD
			if rr, ok := s.ReviewJSON.(*subagent.ReviewResult); ok && rr != nil {
				run.Score = rr.Score
			}
			for _, res := range s.EvalResults {
				if res.Pass {
					run.Passed++
				} else {
					f := res.Check.Kind + " " + res.Check.Arg
					if res.Detail != "" {
						f += ": " + res.Detail
					}
					run.Failed = append(run.Failed, f)
				}
			}
			slog.Info("eval: replay done", "eval", e.Name, "variant", variant, "session_id", id, "passed", run.Passed, "of", run.Total)
			return run
		}
		select {
		case <-ctx.Done():
			run.Status = "abandoned"
			return run
		case <-time.After(poll):
		}
	}
}

// String is the report as posted in a proposal's thread.
func (rep Report) String() string {
	var b strings.Builder
	totals := map[string][2]int{}
	scores := map[string][2]int{} // sum, count
	b.WriteString("| eval | current | proposed |\n|---|---|---|\n")
	byEval := map[string]map[string]Run{}
	var order []string
	for _, run := range rep.Runs {
		if byEval[run.Eval] == nil {
			byEval[run.Eval] = map[string]Run{}
			order = append(order, run.Eval)
		}
		byEval[run.Eval][run.Variant] = run
		t := totals[run.Variant]
		t[0] += run.Passed
		t[1] += run.Total
		totals[run.Variant] = t
		if run.Score > 0 {
			sc := scores[run.Variant]
			sc[0] += run.Score
			sc[1]++
			scores[run.Variant] = sc
		}
	}
	cell := func(run Run, ok bool) string {
		if !ok {
			return "—"
		}
		c := fmt.Sprintf("%d/%d", run.Passed, run.Total)
		if run.Score > 0 {
			c += fmt.Sprintf(", review %d", run.Score)
		}
		if run.Status != string(agent.SessionStatusCompleted) {
			c += " (" + run.Status + ")"
		}
		return c
	}
	for _, name := range order {
		cur, okC := byEval[name][Current]
		pro, okP := byEval[name][Proposed]
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, cell(cur, okC), cell(pro, okP))
	}
	avg := func(v string) string {
		if sc := scores[v]; sc[1] > 0 {
			return fmt.Sprintf(", review %.1f", float64(sc[0])/float64(sc[1]))
		}
		return ""
	}
	fmt.Fprintf(&b, "| **all** | **%d/%d%s** | **%d/%d%s** |\n", totals[Current][0], totals[Current][1], avg(Current), totals[Proposed][0], totals[Proposed][1], avg(Proposed))
	var fails []string
	for _, run := range rep.Runs {
		for _, f := range run.Failed {
			fails = append(fails, fmt.Sprintf("- %s (%s): %s", run.Eval, run.Variant, f))
		}
		if len(run.Failed) > 0 && run.Reply != "" {
			fails = append(fails, fmt.Sprintf("  - its reply: %s", replyLine(run.Reply, 300)))
		}
	}
	if len(fails) > 0 {
		b.WriteString("\nNot passed:\n" + strings.Join(fails[:min(len(fails), 20)], "\n") + "\n")
	}
	fmt.Fprintf(&b, "\n%d replays, $%.2f.", len(rep.Runs), rep.CostUSD)
	if rep.Stopped != "" {
		b.WriteString(" Stopped early: " + rep.Stopped + ".")
	}
	return b.String()
}

// replyLine is a reply on one line, cut to n bytes.
func replyLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
