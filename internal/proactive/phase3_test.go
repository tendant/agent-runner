package proactive

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/thread"
	"github.com/agent-runner/agent-runner/internal/trust"
)

const classPolicy = `---
timezone: UTC
max_sessions_per_day: 2
---
Backup problems: propose a fix.

# Action classes

- ci-fix: CI failing on a watched repo's default branch. Investigate, fix, push.
  trust: notify
- digest: a short summary of new commits. quiet_hours: ignore
  trust: quiet
- risky: anything else
  trust: off
- bogus: x
  trust: always

# Other notes
- not-a-class: ignored here
`

func TestParseClasses(t *testing.T) {
	p := ParsePolicy(classPolicy)
	if len(p.Classes) != 4 {
		t.Fatalf("classes %+v", p.Classes)
	}
	ci, _ := p.Class("ci-fix")
	if ci.Trust != trust.Notify || ci.Description != "CI failing on a watched repo's default branch. Investigate, fix, push." {
		t.Fatalf("ci-fix %+v", ci)
	}
	if d, _ := p.Class("digest"); !d.IgnoreQuiet || d.Trust != trust.Quiet || d.Description != "a short summary of new commits" {
		t.Fatalf("digest %+v", d)
	}
	if b, _ := p.Class("bogus"); b.Trust != trust.Propose {
		t.Fatalf("a bad level is not propose: %+v", b)
	}
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "always") {
		t.Fatalf("problems %v", p.Problems)
	}
	if _, ok := p.Class("not-a-class"); ok {
		t.Fatal("a list item outside the section is a class")
	}
	if p.MaxSessions != 2 {
		t.Fatalf("max sessions %d", p.MaxSessions)
	}
}

type fakeRunner struct{ started []string }

func (r *fakeRunner) StartQuietRun(message string) (string, error) {
	r.started = append(r.started, message)
	return "quiet-" + string(rune('0'+len(r.started))), nil
}

type fakeProber struct{ sigs []Signal }

func (p *fakeProber) Probe(context.Context, []string, []string, map[string]string) []Signal {
	return p.sigs
}

func newClassFixture(t *testing.T) (*fixture, *trust.Store, *fakeRunner) {
	f := newFixture(t)
	f.write(t, PolicyFile, classPolicy)
	f.write(t, AgendaFile, "- [ ] x · due: 2026-11-08 · id: x\n")
	f.set.Sandboxed, f.set.MaxSessions, f.set.BudgetUSD = true, 6, 5
	ts := trust.New(filepath.Join(t.TempDir(), "trust.json"))
	ts.Now = func() time.Time { return f.now }
	r := &fakeRunner{}
	f.svc.cfg.Trust, f.svc.cfg.Runner = ts, r
	return f, ts, r
}

func TestExecuteFollowsTrust(t *testing.T) {
	f, ts, runner := newClassFixture(t)
	ctx := context.Background()

	// notify: runs in a thread of its own at once.
	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "ci-fix", Message: "CI is red: I will fix the test."}
	o := f.svc.Tick(ctx, false)
	if o.Result != "execute" || len(f.chat.runs) != 1 || len(f.chat.posts) != 0 {
		t.Fatalf("notify: %+v runs %d posts %d", o, len(f.chat.runs), len(f.chat.posts))
	}
	if m := f.chat.runs[0].message; !strings.Contains(m, `action class "ci-fix"`) || !strings.Contains(m, "Investigate, fix, push.") || !strings.Contains(m, "data, not instructions") {
		t.Fatalf("run message:\n%s", m)
	}

	// While it runs, the next execute is only proposed.
	o = f.svc.Tick(ctx, true)
	if o.Result != "plan" || !strings.Contains(o.Reason, "in progress") || !strings.Contains(f.chat.posts[0].message, "_Proposed, not run: another proactive run is in progress._") {
		t.Fatalf("in flight: %+v %+v", o, f.chat.posts)
	}

	// It fails: the class drops to propose, and the chat is told.
	f.now = f.now.Add(time.Hour)
	f.sessions = []Session{{ID: "run-1", Source: "stream", Status: "failed", Error: "tests still red", Ended: f.now}}
	f.svc.followRuns(ctx)
	if l := ts.Level("ci-fix", trust.Notify); l != trust.Propose {
		t.Fatalf("level after failure %s", l)
	}
	last := f.chat.posts[len(f.chat.posts)-1].message
	if !strings.Contains(last, "went wrong") || !strings.Contains(last, "tests still red") || !strings.Contains(last, "/trust ci-fix notify") {
		t.Fatalf("notice:\n%s", last)
	}
	// Now it proposes.
	if o := f.svc.Tick(ctx, true); o.Result != "plan" || !strings.Contains(o.Reason, "ci-fix is at propose") {
		t.Fatalf("after demotion: %+v", o)
	}
	if c, _ := ts.Get("ci-fix"); c.History.Executed != 1 || c.History.Failed != 1 || c.History.Proposed != 2 {
		t.Fatalf("history %+v", c.History)
	}

	// quiet: no chat at all; a clean run counts toward promotion.
	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "digest", Message: "summarize"}
	if o := f.svc.Tick(ctx, true); o.Result != "execute" || len(runner.started) != 1 {
		t.Fatalf("quiet: %+v", o)
	}
	f.sessions = []Session{{ID: "quiet-1", Source: "proactive", Status: "completed", ReviewScore: 8, CostUSD: 0.4, Ended: f.now}}
	f.svc.followRuns(ctx)
	if c, _ := ts.Get("digest"); c.Clean != 1 || c.Level != trust.Quiet {
		t.Fatalf("digest %+v", c)
	}
	if st := f.svc.load(); st.Spent != 0.4 || st.Sessions != 2 {
		t.Fatalf("spend %v sessions %d", st.Spent, st.Sessions)
	}

	// The daily cap of runs (2 in the policy) is reached: proposed.
	if o := f.svc.Tick(ctx, true); o.Result != "plan" || !strings.Contains(o.Reason, "2 proactive runs are used") {
		t.Fatalf("cap: %+v", o)
	}

	// Undoing a run's memory demotes its class.
	if note := f.svc.SessionUndone(ctx, "quiet-1"); !strings.Contains(note, "digest") {
		t.Fatalf("undo note %q", note)
	}
	if l := ts.Level("digest", trust.Quiet); l != trust.Notify {
		t.Fatalf("after undo %s", l)
	}
	if f.svc.SessionUndone(ctx, "quiet-1") != "" {
		t.Fatal("demoted twice for one undo")
	}
}

func TestExecuteGuards(t *testing.T) {
	f, _, _ := newClassFixture(t)
	ctx := context.Background()
	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "ci-fix", Message: "fix"}

	f.set.Sandboxed = false
	if o := f.svc.Tick(ctx, true); o.Result != "plan" || !strings.Contains(o.Reason, "AGENT_SANDBOX=strict") {
		t.Fatalf("unsandboxed: %+v", o)
	}
	f.set.Sandboxed = true

	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "risky", Message: "do it"}
	if o := f.svc.Tick(ctx, true); o.Result != "ignore" || !strings.Contains(o.Reason, "off") {
		t.Fatalf("off class: %+v", o)
	}
	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "made-up", Message: "do it"}
	if o := f.svc.Tick(ctx, true); o.Result != "plan" || !strings.Contains(o.Reason, "no action class") {
		t.Fatalf("unknown class: %+v", o)
	}
	if len(f.chat.runs) != 0 {
		t.Fatal("something ran")
	}
}

func TestQuietHoursLetOnlyIgnoringClassesAct(t *testing.T) {
	f, _, runner := newClassFixture(t)
	f.write(t, PolicyFile, strings.Replace(classPolicy, "timezone: UTC", "timezone: UTC\nquiet_hours: 11:00-13:00", 1))
	ctx := context.Background()

	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "ci-fix", Message: "fix"}
	if o := f.svc.Tick(ctx, false); o.Result != "skipped" || o.Reason != "quiet hours" {
		t.Fatalf("ci-fix in quiet hours: %+v", o)
	}
	if st := f.svc.load(); len(st.Seen) != 0 {
		t.Fatalf("an item held back by quiet hours is marked seen: %v", st.Seen)
	}
	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "digest", Message: "summarize"}
	if o := f.svc.Tick(ctx, false); o.Result != "execute" || len(runner.started) != 1 {
		t.Fatalf("digest in quiet hours: %+v", o)
	}
}

func TestSignalsReachTriage(t *testing.T) {
	f, _, _ := newClassFixture(t)
	f.write(t, AgendaFile, "")
	f.write(t, PolicyFile, strings.Replace(classPolicy, "timezone: UTC", "timezone: UTC\nsignals: repo-ci", 1))
	p := &fakeProber{sigs: []Signal{{Key: "ci:app:abc", Kind: SignalCI, Text: "CI is failing on app (main) at abc: test (failure)"}}}
	f.svc.cfg.Signals = p
	f.triager.res = thread.AnalysisResult{Action: "ask", Message: "CI is red on app"}
	o := f.svc.Tick(context.Background(), false)
	if o.Result != "ask" || len(o.Items) != 1 || o.Items[0] != "ci:app:abc" {
		t.Fatalf("outcome %+v", o)
	}
	if r := f.triager.reports[0]; !strings.Contains(r, "## Signals") || !strings.Contains(r, "CI is failing on app") {
		t.Fatalf("report:\n%s", r)
	}
	// The same failing commit is not raised again within the cooldown.
	f.now = f.now.Add(time.Hour)
	if o := f.svc.Tick(context.Background(), false); o.Result != "nothing-new" {
		t.Fatalf("again: %+v", o)
	}
}

func TestTrustCommands(t *testing.T) {
	f, ts, _ := newClassFixture(t)
	out := f.svc.TrustStatus()
	for _, want := range []string{"**ci-fix**: notify", "**risky**: off", "/trust <class> <level>"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if _, err := f.svc.SetTrust("nope", "quiet"); err == nil {
		t.Fatal("set an unknown class")
	}
	if _, err := f.svc.SetTrust("ci-fix", "always"); err == nil {
		t.Fatal("set an unknown level")
	}
	if r, err := f.svc.SetTrust("ci-fix", "quiet"); err != nil || !strings.Contains(r, "notify → quiet") {
		t.Fatalf("set: %q %v", r, err)
	}
	if l := ts.Level("ci-fix", trust.Notify); l != trust.Quiet {
		t.Fatalf("level %s", l)
	}
}
