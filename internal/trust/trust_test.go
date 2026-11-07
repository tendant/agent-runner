package trust

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLadder(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "trust.json"))
	if l := s.Level("ci-fix", Notify); l != Notify {
		t.Fatalf("seed %s", l)
	}
	// Seeded once: a later PROACTIVE.md edit does not reset the live level.
	if l := s.Level("ci-fix", Quiet); l != Notify {
		t.Fatalf("reseeded to %s", l)
	}
	for range PromoteAfter {
		s.Executed("ci-fix")
		s.Succeeded("ci-fix")
	}
	c, _ := s.Get("ci-fix")
	if got := c.Suggestion("ci-fix"); !strings.Contains(got, "/trust ci-fix quiet") {
		t.Fatalf("suggestion %q", got)
	}
	if from, to := s.Failed("ci-fix", "session failed"); from != Notify || to != Propose {
		t.Fatalf("demote %s -> %s", from, to)
	}
	c, _ = s.Get("ci-fix")
	if c.Clean != 0 || c.History.Failed != 1 || c.History.Demoted != 1 || c.Reason != "session failed" || c.Suggestion("ci-fix") != "" {
		t.Fatalf("after failure %+v", c)
	}
	s.Demote("ci-fix", "x")
	if _, to := s.Demote("ci-fix", "y"); to != Off {
		t.Fatalf("floor %s", to)
	}
	if from := s.Set("ci-fix", Notify, "operator"); from != Off {
		t.Fatalf("set from %s", from)
	}
	again := New(s.path)
	if l := again.Level("ci-fix", Propose); l != Notify {
		t.Fatalf("not persisted: %s", l)
	}
	if !Notify.Executes() || Propose.Executes() {
		t.Fatal("Executes")
	}
	if _, ok := ParseLevel("QUIET"); !ok {
		t.Fatal("ParseLevel")
	}
	if _, ok := ParseLevel("always"); ok {
		t.Fatal("ParseLevel accepted junk")
	}
}
