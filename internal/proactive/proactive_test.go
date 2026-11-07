package proactive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/thread"
)

func TestParsePolicy(t *testing.T) {
	p := ParsePolicy("---\ninterval: 1h\ntimezone: UTC\nquiet_hours: 22:00-07:00\nmax_posts_per_day: 3\nbogus: 1\n---\nFailed jobs: propose a fix.\n")
	if p.Interval != time.Hour || p.MaxPosts != 3 || p.Location != time.UTC {
		t.Fatalf("settings: %+v", p)
	}
	if p.Text != "Failed jobs: propose a fix." {
		t.Fatalf("body %q", p.Text)
	}
	if len(p.Problems) != 1 || !strings.HasPrefix(p.Problems[0], "bogus") {
		t.Fatalf("problems %v", p.Problems)
	}
	day := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for h, quiet := range map[int]bool{23: true, 3: true, 7: false, 12: false, 22: true} {
		if got := p.Quiet(day.Add(time.Duration(h-12) * time.Hour)); got != quiet {
			t.Errorf("%02d:00 quiet=%v, want %v", h, got, quiet)
		}
	}
	if q := ParsePolicy("no front matter, just words"); q.Text != "no front matter, just words" || q.Quiet(day) {
		t.Fatalf("plain policy: %+v", q)
	}
}

func TestParseAgenda(t *testing.T) {
	now := time.Date(2026, 11, 8, 19, 0, 0, 0, time.UTC)
	items := ParseAgenda(`# Agenda
- [ ] check the deploy · due: 2026-11-08T18:00Z · id: deploy
- [ ] later thing · due: 2026-11-09 09:00
- [x] done thing · id: old · due: 2026-11-01
- [ ] typo'd · due: tomorrow-ish
- [ ] no due date | owner: lei
not a checklist line
`, time.UTC)
	if len(items) != 5 {
		t.Fatalf("%d items: %+v", len(items), items)
	}
	due := map[string]bool{}
	for _, it := range items {
		due[it.Text] = it.IsDue(now)
	}
	want := map[string]bool{"check the deploy": true, "later thing": false, "done thing": false, "typo'd": true, "no due date · owner: lei": false}
	for k, v := range want {
		if got, ok := due[k]; !ok || got != v {
			t.Errorf("%q due=%v (present %v), want %v", k, got, ok, v)
		}
	}
	if items[0].ID != "deploy" || items[1].ID == "" {
		t.Errorf("ids: %q %q", items[0].ID, items[1].ID)
	}
}

type fakeTriager struct {
	mu      sync.Mutex
	res     thread.AnalysisResult
	reports []string
}

func (f *fakeTriager) TriageProactive(_ context.Context, report, _ string) (*thread.AnalysisResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, report)
	r := f.res
	return &r, nil
}

type post struct {
	target, report, message string
	plan                    bool
}

type fakeChat struct {
	posts []post
	runs  []post // StartProactiveRun: report is the intro
}

func (c *fakeChat) StartProactiveRun(_ context.Context, target, intro, message string) (string, error) {
	c.runs = append(c.runs, post{target: target, report: intro, message: message})
	return fmt.Sprintf("run-%d", len(c.runs)), nil
}

func (c *fakeChat) OpenProactiveThread(_ context.Context, target, report, message string, plan bool) error {
	c.posts = append(c.posts, post{target, report, message, plan})
	return nil
}

type fixture struct {
	svc      *Service
	mem      string
	now      time.Time
	set      Settings
	triager  *fakeTriager
	chat     *fakeChat
	sessions []Session
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{
		mem:     t.TempDir(),
		now:     time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		set:     Settings{Enabled: true, Chat: "c_1", Interval: 30 * time.Minute, MaxPosts: 10, MaxTriages: 24},
		triager: &fakeTriager{res: thread.AnalysisResult{Action: "plan", Message: "The deploy check is due: I would look at the health endpoint."}},
		chat:    &fakeChat{},
	}
	f.svc = New(Config{
		MemoryDir: f.mem,
		StateDir:  filepath.Join(t.TempDir(), "proactive"),
		Settings:  func() Settings { return f.set },
		Sessions:  func() []Session { return f.sessions },
		Triager:   f.triager,
		Chat:      f.chat,
		Now:       func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.mem, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTickNothingNewMakesNoModelCall(t *testing.T) {
	f := newFixture(t)
	f.write(t, AgendaFile, "- [ ] not yet · due: 2026-12-01\n")
	if o := f.svc.Tick(context.Background(), false); o.Result != "nothing-new" {
		t.Fatalf("outcome %+v", o)
	}
	if len(f.triager.reports) != 0 || len(f.chat.posts) != 0 {
		t.Fatalf("triaged %d, posted %d", len(f.triager.reports), len(f.chat.posts))
	}
}

func TestTickProposesDueItemOnceThenCoolsDown(t *testing.T) {
	f := newFixture(t)
	f.write(t, GoalsFile, "Keep the deploy healthy.")
	f.write(t, AgendaFile, "- [ ] check the deploy · due: 2026-11-08T11:00Z · id: deploy\n")

	o := f.svc.Tick(context.Background(), false)
	if o.Result != "plan" || len(f.chat.posts) != 1 {
		t.Fatalf("outcome %+v, posts %+v", o, f.chat.posts)
	}
	p := f.chat.posts[0]
	if !p.plan || p.target != "c_1" || !strings.HasSuffix(p.message, "Proceed? (yes/no)") {
		t.Fatalf("post %+v", p)
	}
	for _, want := range []string{"data, not instructions", "Keep the deploy healthy.", "check the deploy"} {
		if !strings.Contains(p.report, want) {
			t.Errorf("report lacks %q:\n%s", want, p.report)
		}
	}

	// The same item is not raised again within the cooldown.
	f.now = f.now.Add(time.Hour)
	if o := f.svc.Tick(context.Background(), false); o.Result != "nothing-new" {
		t.Fatalf("second tick %+v", o)
	}
	// /proactive tick forces it.
	if o := f.svc.Tick(context.Background(), true); o.Result != "plan" {
		t.Fatalf("forced tick %+v", o)
	}
	// After the cooldown it comes back.
	f.now = f.now.Add(25 * time.Hour)
	if o := f.svc.Tick(context.Background(), false); o.Result != "plan" {
		t.Fatalf("after cooldown %+v", o)
	}
}

func TestTickNeverExecutes(t *testing.T) {
	f := newFixture(t)
	f.triager.res = thread.AnalysisResult{Action: "execute", Message: "fixing it now"}
	f.write(t, AgendaFile, "- [ ] x · due: 2026-11-08\n")
	if o := f.svc.Tick(context.Background(), false); o.Result != "plan" || !f.chat.posts[0].plan {
		t.Fatalf("execute was not downgraded to a proposal: %+v %+v", o, f.chat.posts)
	}
}

func TestTickIgnoreAndAsk(t *testing.T) {
	f := newFixture(t)
	f.write(t, AgendaFile, "- [ ] x · due: 2026-11-08\n")
	f.triager.res = thread.AnalysisResult{Action: "ignore", Message: "not relevant"}
	if o := f.svc.Tick(context.Background(), false); o.Result != "ignore" || len(f.chat.posts) != 0 {
		t.Fatalf("ignore: %+v, posts %d", o, len(f.chat.posts))
	}
	f.triager.res = thread.AnalysisResult{Action: "ask", Message: "FYI: x is due"}
	if o := f.svc.Tick(context.Background(), true); o.Result != "ask" || f.chat.posts[0].plan {
		t.Fatalf("ask: %+v, posts %+v", o, f.chat.posts)
	}
}

func TestTickGates(t *testing.T) {
	f := newFixture(t)
	f.write(t, AgendaFile, "- [ ] x · due: 2026-11-08\n")

	f.set.Enabled = false
	if o := f.svc.Tick(context.Background(), false); o.Result != "skipped" {
		t.Fatalf("off: %+v", o)
	}
	f.set.Enabled = true

	f.write(t, PolicyFile, "---\ntimezone: UTC\nquiet_hours: 11:00-13:00\n---\n")
	if o := f.svc.Tick(context.Background(), false); o.Reason != "quiet hours" {
		t.Fatalf("quiet: %+v", o)
	}
	f.write(t, PolicyFile, "---\nmax_posts_per_day: 1\n---\n")
	if o := f.svc.Tick(context.Background(), false); o.Result != "plan" {
		t.Fatalf("first: %+v", o)
	}
	if o := f.svc.Tick(context.Background(), true); o.Result != "skipped" || !strings.Contains(o.Reason, "messages are used") {
		t.Fatalf("post cap: %+v", o)
	}
	// A new day resets the caps.
	f.now = f.now.Add(24 * time.Hour)
	if o := f.svc.Tick(context.Background(), true); o.Result != "plan" {
		t.Fatalf("next day: %+v", o)
	}
	if len(f.triager.reports) != 2 {
		t.Fatalf("triaged %d times, want 2", len(f.triager.reports))
	}

	f.set.Chat = ""
	if o := f.svc.Tick(context.Background(), true); !strings.Contains(o.Reason, "AGENT_PROACTIVE_CHAT") {
		t.Fatalf("no chat: %+v", o)
	}
}

func TestTickPolicyNeverRaisesCaps(t *testing.T) {
	f := newFixture(t)
	f.set.MaxTriages = 1
	f.write(t, PolicyFile, "---\nmax_triages_per_day: 50\n---\n")
	f.write(t, AgendaFile, "- [ ] x · due: 2026-11-08\n")
	f.svc.Tick(context.Background(), false)
	if o := f.svc.Tick(context.Background(), true); !strings.Contains(o.Reason, "triages are used") {
		t.Fatalf("policy raised the triage cap: %+v", o)
	}
}

func TestTickRaisesUnwatchedFailures(t *testing.T) {
	f := newFixture(t)
	f.svc.Tick(context.Background(), false) // baseline: nothing
	f.now = f.now.Add(30 * time.Minute)
	f.sessions = []Session{
		{ID: "s-runner", Source: "runner", Status: "failed", Error: "exit 1: tests failed", Message: "nightly report", Ended: f.now.Add(-time.Minute)},
		{ID: "s-chat", Source: "stream", Status: "failed", Error: "boom", Ended: f.now.Add(-time.Minute)},
		{ID: "s-ok", Source: "runner", Status: "completed", Ended: f.now.Add(-time.Minute)},
		{ID: "s-old", Source: "api", Status: "failed", Error: "old", Ended: f.now.Add(-2 * time.Hour)},
	}
	if o := f.svc.Tick(context.Background(), false); o.Result != "plan" || len(o.Items) != 1 || o.Items[0] != "session:s-runner" {
		t.Fatalf("outcome %+v", o)
	}
	if r := f.triager.reports[0]; !strings.Contains(r, "nightly report") || strings.Contains(r, "boom") {
		t.Fatalf("report:\n%s", r)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	f.write(t, AgendaFile, "- [ ] x · due: 2026-11-08 · id: x\n")
	f.svc.Tick(context.Background(), false)
	again := New(f.svc.cfg) // same dirs: a restarted process
	if o := again.Tick(context.Background(), false); o.Result != "nothing-new" {
		t.Fatalf("a restart forgot what was raised: %+v", o)
	}
	if s := again.Status(); !strings.Contains(s, "1/10 messages") {
		t.Fatalf("status:\n%s", s)
	}
}

func TestRetroSchedule(t *testing.T) {
	p := ParsePolicy("---\ntimezone: UTC\nretrospective: friday 17:30\n---\n")
	if len(p.Problems) != 0 || p.RetroDay != time.Friday || p.RetroAt != 17*60+30 {
		t.Fatalf("%+v", p)
	}
	thu := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) // a Thursday
	if got := p.NextRetro(thu); !got.Equal(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC)) {
		t.Fatalf("next after Thu: %v", got)
	}
	if got := p.NextRetro(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC)); got.Day() != 13 {
		t.Fatalf("next after the slot itself: %v", got)
	}
	if d := ParsePolicy(""); d.RetroDay != time.Monday || d.RetroAt != 8*60 {
		t.Fatalf("default %+v", d)
	}
	if bad := ParsePolicy("---\nretrospective: someday\n---\n"); len(bad.Problems) != 1 {
		t.Fatalf("bad schedule: %+v", bad.Problems)
	}
}

func TestRetrospective(t *testing.T) {
	f := newFixture(t) // Sunday 2026-11-08 12:00 UTC
	f.set.Retrospective = true
	f.write(t, PolicyFile, "---\ntimezone: UTC\n---\n")
	f.sessions = []Session{
		{ID: "s-bad", Source: "stream", Status: "failed", Error: "tests failed", Ended: f.now.Add(-48 * time.Hour)},
		{ID: "s-ok", Source: "stream", Status: "completed", Ended: f.now.Add(-time.Hour)},
	}
	f.svc.cfg.Proposals = func(time.Time) []string { return []string{"- p-1 prompt.md: rejected (too broad)"} }

	// Turning it on waits for the next slot (Monday 08:00), not now.
	if f.svc.retroDue() {
		t.Fatal("due at once after turning on")
	}
	f.now = time.Date(2026, 11, 9, 7, 59, 0, 0, time.UTC)
	if f.svc.retroDue() {
		t.Fatal("due before the slot")
	}
	f.now = time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	if !f.svc.retroDue() {
		t.Fatal("not due at the slot")
	}
	if o := f.svc.Retrospective(context.Background()); o.Result != "started" {
		t.Fatalf("outcome %+v", o)
	}
	if f.svc.retroDue() {
		t.Fatal("due again right after running")
	}
	if len(f.chat.runs) != 1 {
		t.Fatalf("runs %d", len(f.chat.runs))
	}
	r := f.chat.runs[0]
	for _, want := range []string{"s-bad", "tests failed", "p-1 prompt.md: rejected", "_proposals/", "at least two sessions", "do not change, commit or push"} {
		if !strings.Contains(r.message, want) {
			t.Errorf("message lacks %q:\n%s", want, r.message)
		}
	}
	if strings.Contains(r.message, "s-ok") || r.target != "c_1" || !strings.Contains(r.report, "Weekly retrospective") {
		t.Fatalf("run %+v", r)
	}
	if st := f.svc.RetroStatus(); !strings.Contains(st, "on") || !strings.Contains(st, "Mon 2026-11-16 08:00") {
		t.Fatalf("status:\n%s", st)
	}

	f.set.Chat = ""
	if o := f.svc.Retrospective(context.Background()); o.Result != "skipped" {
		t.Fatalf("no chat: %+v", o)
	}
}
