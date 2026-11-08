package evals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/subagent"
)

func TestParse(t *testing.T) {
	e := Parse("lint", "---\nfile: hello.go\ncommand: go vet ./...\nreply: gofmt\nbogus: x\n---\nAdd hello.go.\n")
	if e.Message != "Add hello.go." || len(e.Checks) != 3 || e.Checks[1] != (agent.EvalCheck{Kind: "command", Arg: "go vet ./..."}) {
		t.Fatalf("%+v", e)
	}
	if len(e.Problems) != 1 || !e.Usable() {
		t.Fatalf("problems %v", e.Problems)
	}
	if e := Parse("x", "just a task"); e.Usable() || len(e.Problems) != 1 {
		t.Fatalf("no checks: %+v", e)
	}
}

func TestSet(t *testing.T) {
	mem := t.TempDir()
	if _, err := Write(mem, "Bad Name", "---\nreply: x\n---\ntask"); err == nil {
		t.Fatal("bad name saved")
	}
	if _, err := Write(mem, "empty", "---\n---\n"); err == nil {
		t.Fatal("an unusable eval saved")
	}
	if _, err := Write(mem, "a", "---\nreply: x\n---\ntask a"); err != nil {
		t.Fatal(err)
	}
	set, _ := List(mem)
	if len(set) != 1 || set[0].Message != "task a" {
		t.Fatalf("set %+v", set)
	}
	if ok, err := Remove(mem, "a"); !ok || err != nil {
		t.Fatalf("remove %v %v", ok, err)
	}
	if set, _ := List(mem); len(set) != 0 {
		t.Fatal("not removed")
	}
}

// fakeEngine finishes each replay at once: the proposed variant passes one
// more check than the current one.
type fakeEngine struct {
	mu       sync.Mutex
	sessions map[string]*agent.Session
	specs    []*agent.EvalSpec
}

func (f *fakeEngine) start(message string, spec *agent.EvalSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("s%d", len(f.specs))
	f.specs = append(f.specs, spec)
	var res []agent.EvalResult
	for i, c := range spec.Checks {
		res = append(res, agent.EvalResult{Check: c, Pass: i == 0 || spec.Overrides != nil, Detail: "nope"})
	}
	score := 5
	if spec.Overrides != nil {
		score = 8
	}
	f.sessions[id] = &agent.Session{ID: id, Status: agent.SessionStatusCompleted, EvalResults: res, TotalCostUSD: 0.5, ReviewJSON: &subagent.ReviewResult{Score: score},
		Iterations: []agent.IterationResult{{Output: "I fixed the\ncode."}, {Output: ""}}}
	return id, nil
}

func (f *fakeEngine) get(id string) (*agent.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	return s, ok
}

func TestCompare(t *testing.T) {
	mem := t.TempDir()
	Write(mem, "a", "---\nfile: x\nreply: y\n---\ntask a")
	Write(mem, "b", "---\nfile: x\nreply: y\n---\ntask b")
	f := &fakeEngine{sessions: map[string]*agent.Session{}}
	left := 1.6
	r := &Runner{MemoryDir: mem, Start: f.start, Get: f.get, Poll: time.Millisecond,
		BudgetLeft: func() float64 { return left }, Spend: func(usd float64) { left -= usd }}

	rep, err := r.Compare(context.Background(), map[string]string{"prompt.md": "new"})
	if err != nil {
		t.Fatal(err)
	}
	// $1.60 lasts four replays at $0.50: the set's two evals, both variants.
	if len(rep.Runs) != 4 || rep.Stopped != "" {
		t.Fatalf("runs %d stopped %q", len(rep.Runs), rep.Stopped)
	}
	if f.specs[0].Overrides != nil || f.specs[1].Overrides["prompt.md"] != "new" {
		t.Fatal("the variants are not current then proposed")
	}
	out := rep.String()
	for _, want := range []string{"| a | 1/2, review 5 | 2/2, review 8 |", "| **all** | **2/4, review 5.0** | **4/4, review 8.0** |", "a (current): reply y: nope", "  - its reply: I fixed the code.", "4 replays, $2.00."} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	// Spent: the next comparison does not start.
	if _, err := r.Check(); err == nil || !strings.Contains(err.Error(), "spent") {
		t.Fatalf("check after the budget: %v", err)
	}

	left = 0.7 // one replay's worth
	rep, _ = (&Runner{MemoryDir: mem, Start: f.start, Get: f.get, Poll: time.Millisecond,
		BudgetLeft: func() float64 { return left }, Spend: func(usd float64) { left -= usd }}).Compare(context.Background(), nil)
	if len(rep.Runs) != 2 || !strings.Contains(rep.Stopped, "spent") {
		t.Fatalf("budget stop: %d runs, %q", len(rep.Runs), rep.Stopped)
	}

	r.Ready = func() error { return errors.New("no sandbox") }
	left = 10
	if _, err := r.Compare(context.Background(), nil); err == nil || err.Error() != "no sandbox" {
		t.Fatalf("not ready: %v", err)
	}
	if _, err := (&Runner{MemoryDir: t.TempDir()}).Check(); err == nil {
		t.Fatal("an empty set can be compared")
	}
}
