package task

import (
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStore_SaveGetUpdateDelete(t *testing.T) {
	s := newTestStore(t)
	if _, ok := s.Get("m_1"); ok {
		t.Fatal("Get on an empty store found a record")
	}
	rec := New("m_1", "fix the bug", "/ws/a")
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("m_1")
	if !ok || got.ID != rec.ID || got.Goal != "fix the bug" || got.Status != StatusWorking {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	found, err := s.Update("m_1", func(r *Record) { r.Status = StatusDone })
	if !found || err != nil {
		t.Fatalf("Update = %v, %v", found, err)
	}
	if got, _ := s.Get("m_1"); got.Status != StatusDone {
		t.Fatalf("status after Update = %s", got.Status)
	}
	if found, _ := s.Update("m_missing", func(*Record) {}); found {
		t.Fatal("Update on a missing record reported found")
	}
	s.Delete("m_1")
	if _, ok := s.Get("m_1"); ok {
		t.Fatal("record still there after Delete")
	}
}

func TestStore_KeysAreSanitized(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(New("../../etc/passwd", "x", "")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("../../etc/passwd"); !ok {
		t.Fatal("record with an unsafe key not found by the same key")
	}
	if got := FileKey("a/b:c"); got != "a_b_c" {
		t.Fatalf("FileKey = %q", got)
	}
}

func backdate(t *testing.T, s *Store, key string, status Status, age time.Duration) {
	t.Helper()
	rec, _ := s.Get(key)
	rec.Status = status
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	// Save stamps UpdatedAt; rewrite it directly.
	rec.UpdatedAt = time.Now().Add(-age)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeRaw(rec); err != nil {
		t.Fatal(err)
	}
}

func TestSweep(t *testing.T) {
	s := newTestStore(t)
	for _, k := range []string{"done-old", "done-new", "idle-old", "idle-new", "working-old"} {
		if err := s.Save(New(k, k, "/ws/"+k)); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, s, "done-old", StatusDone, 25*time.Hour)
	backdate(t, s, "done-new", StatusDone, time.Hour)
	backdate(t, s, "idle-old", StatusAwaitingInput, 8*24*time.Hour)
	backdate(t, s, "idle-new", StatusPaused, 24*time.Hour)
	backdate(t, s, "working-old", StatusWorking, 30*24*time.Hour)

	var finished []string
	s.Sweep(time.Now(), SweepPolicy{
		Retention: 24 * time.Hour,
		IdleTTL:   7 * 24 * time.Hour,
		Finish:    func(ws string) { finished = append(finished, ws) },
	})

	if _, ok := s.Get("done-old"); ok {
		t.Error("done task past retention kept")
	}
	if _, ok := s.Get("done-new"); !ok {
		t.Error("done task within retention removed")
	}
	if rec, _ := s.Get("idle-old"); rec.Status != StatusFailed || rec.FailReason != "expired" || !rec.WorkspaceRemoved {
		t.Errorf("idle task past TTL = %s/%q removed=%v, want failed/expired", rec.Status, rec.FailReason, rec.WorkspaceRemoved)
	}
	if rec, _ := s.Get("idle-new"); rec.Status != StatusPaused {
		t.Errorf("idle task within TTL = %s, want untouched", rec.Status)
	}
	if rec, _ := s.Get("working-old"); rec.Status != StatusWorking {
		t.Errorf("working task touched by sweep: %s", rec.Status)
	}
	want := map[string]bool{"/ws/done-old": true, "/ws/idle-old": true}
	if len(finished) != 2 || !want[finished[0]] || !want[finished[1]] {
		t.Errorf("finished workspaces = %v, want done-old and idle-old", finished)
	}
}

func TestContextBlock(t *testing.T) {
	rec := New("m_1", "deploy the app", "")
	rec.Plan = []PlanStep{{ID: "1", Text: "build", Done: true}, {ID: "2", Text: "deploy"}}
	rec.Decisions = []string{"use Postgres"}
	rec.Turns = []Turn{{Summary: "built the image", Question: "Which environment?"}}
	got := ContextBlock(rec, " staging ", false)
	for _, want := range []string{
		"## Task", "Goal: deploy the app", "1 [done] build", "2 [todo] deploy",
		"- use Postgres", `turn 1 — built the image (asked: "Which environment?")`, "User's reply: staging",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("context block missing %q:\n%s", want, got)
		}
	}
}

func TestAddDecisionsDedupes(t *testing.T) {
	rec := New("m_1", "g", "")
	rec.AddDecisions([]string{"a", " a ", "", "b"})
	rec.AddDecisions([]string{"b", "c"})
	if strings.Join(rec.Decisions, ",") != "a,b,c" {
		t.Fatalf("decisions = %v", rec.Decisions)
	}
}

func TestLimitsExceeded(t *testing.T) {
	l := Limits{MaxTurns: 3, MaxSeconds: 3600, MaxCostUSD: 2}
	if got := l.Exceeded(Budget{Turns: 2, Seconds: 100, CostUSD: 1}); got != "" {
		t.Errorf("within limits: %q", got)
	}
	for b, want := range map[Budget]string{
		{Turns: 3}:      "3 turns",
		{Seconds: 3600}: "1h0m0s of work",
		{CostUSD: 2.5}:  "$2.50 spent",
	} {
		if got := l.Exceeded(b); got != want {
			t.Errorf("Exceeded(%+v) = %q, want %q", b, got, want)
		}
	}
	if got := (Limits{}).Exceeded(Budget{Turns: 100, Seconds: 1e6, CostUSD: 1e3}); got != "" {
		t.Errorf("zero limits should be unlimited, got %q", got)
	}
}

func TestContextBlockFeedback(t *testing.T) {
	got := ContextBlock(New("m", "g", ""), "add tests", true)
	if !strings.Contains(got, "User's feedback on the task (address it): add tests") {
		t.Errorf("feedback label missing:\n%s", got)
	}
}
