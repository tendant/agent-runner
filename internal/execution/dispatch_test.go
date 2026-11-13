package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// startRemote runs s on a fake remote backend and returns the spec the
// backend got and a channel closed when the worker lets go of s.
func startRemote(t *testing.T, env *engineTestEnv, s *agent.Session) (Spec, <-chan struct{}) {
	t.Helper()
	fb := &fakeBackend{started: make(chan Spec, 1)}
	env.handlers.SetBackend(fb)
	env.handlers.SetJobDispatcherURL("http://127.0.0.1:1")
	returned := make(chan struct{})
	go func() { env.handlers.RunQueued(s); close(returned) }()
	select {
	case spec := <-fb.started:
		return spec, returned
	case <-time.After(5 * time.Second):
		t.Fatal("backend never started")
	}
	return Spec{}, nil
}

// A remote runner gets a token only for its own session, and the session
// to run; a token stops working for a session that is not running remotely.
func TestJobSpecAndToken(t *testing.T) {
	env := setupTestEnv(t)
	e := env.handlers.Engine
	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "alice", "", 3, 60)
	other, _ := env.handlers.agentManager.CreateSession("other", nil, "bob", "", 3, 60)
	spec, _ := startRemote(t, env, s)

	if spec.Token == "" || spec.Token != e.SessionToken(s.ID) || spec.ReportURL != "http://127.0.0.1:1" {
		t.Fatalf("spec %+v", spec)
	}
	if e.SessionToken(s.ID) == e.SessionToken(other.ID) {
		t.Fatal("two sessions share a token")
	}
	w, err := e.JobSpec(s.ID, spec.Token)
	if err != nil || w.ID != s.ID || w.Message != "do it" {
		t.Fatalf("spec %+v err %v", w, err)
	}
	for name, try := range map[string]func() error{
		"bad token":        func() error { _, err := e.JobSpec(s.ID, "x"); return err },
		"other's token":    func() error { _, err := e.JobSpec(s.ID, e.SessionToken(other.ID)); return err },
		"not a remote run": func() error { _, err := e.JobSpec(other.ID, e.SessionToken(other.ID)); return err },
		"unknown session":  func() error { _, err := e.JobSpec("agent-nope", e.SessionToken("agent-nope")); return err },
	} {
		if err := try(); !errors.Is(err, ErrJobAuth) {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

// Reports apply in sequence: a stale or repeated one changes nothing.
func TestJobReportSequence(t *testing.T) {
	env := setupTestEnv(t)
	e := env.handlers.Engine
	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "alice", "", 3, 60)
	spec, _ := startRemote(t, env, s)

	report := func(seq uint64, iter int) error {
		w, _ := s.Wire()
		w.CurrentIteration = iter
		_, err := e.JobReport(s.ID, spec.Token, &JobReport{Seq: seq, Session: w})
		return err
	}
	if err := report(2, 2); err != nil {
		t.Fatal(err)
	}
	if err := report(1, 1); err != nil { // older, arriving late
		t.Fatal(err)
	}
	if err := report(2, 9); err != nil { // a retry of seq 2
		t.Fatal(err)
	}
	if got := s.Snapshot().CurrentIteration; got != 2 {
		t.Errorf("iteration %d, want 2", got)
	}
	if _, err := e.JobReport(s.ID, spec.Token, &JobReport{Seq: 3}); err == nil {
		t.Error("accepted a report without a session")
	}
}

// Control returns a stop or queued steer messages at once, and otherwise
// waits until one comes or the wait is up.
func TestJobControl(t *testing.T) {
	env := setupTestEnv(t)
	e := env.handlers.Engine
	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "alice", "", 3, 60)
	spec, _ := startRemote(t, env, s)
	ctx := context.Background()

	start := time.Now()
	c, err := e.JobControl(ctx, s.ID, spec.Token, 50*time.Millisecond)
	if err != nil || c.Stop || len(c.Steer) != 0 || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("idle control %+v err %v after %v", c, err, time.Since(start))
	}

	// Steer reaches a remote run only once its runner says its CLI takes
	// it; until then it fails, and the chat falls back to a follow-up run.
	if err := e.Steer(s.ID, "early"); err == nil {
		t.Error("steer queued for a runner that has not said it can take it")
	}
	w, _ := s.Wire()
	if _, err := e.JobReport(s.ID, spec.Token, &JobReport{Seq: 1, Session: w, Steerable: true}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = e.Steer(s.ID, "use main")
	}()
	c, err = e.JobControl(ctx, s.ID, spec.Token, 5*time.Second)
	if err != nil || len(c.Steer) != 1 || c.Steer[0] != "use main" {
		t.Fatalf("steer control %+v err %v", c, err)
	}
	// Delivered once.
	if c, _ = e.JobControl(ctx, s.ID, spec.Token, 0); len(c.Steer) != 0 {
		t.Errorf("steer delivered twice: %+v", c)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = env.handlers.agentManager.StopSession(s.ID)
	}()
	start = time.Now()
	c, err = e.JobControl(ctx, s.ID, spec.Token, 5*time.Second)
	if err != nil || !c.Stop || time.Since(start) > 2*time.Second {
		t.Fatalf("stop control %+v err %v after %v", c, err, time.Since(start))
	}
}

// The result is taken once: the session gets its final state and is
// finalized here, the worker lets go of it, and the runner's further
// requests are told the run has ended.
func TestJobResult(t *testing.T) {
	env := setupTestEnv(t)
	e := env.handlers.Engine
	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "alice", "", 3, 60)
	spec, returned := startRemote(t, env, s)

	// The runner's own copy of the session, run to completion.
	w, _ := s.Wire()
	runner, err := agent.NewSessionFromWire(w, DecodeWire)
	if err != nil {
		t.Fatal(err)
	}
	runner.AddIteration(agent.IterationResult{Iteration: 1, Status: agent.IterationStatusSuccess, Output: "done"})
	runner.Complete()
	res := &RunResult{SessionID: s.ID, Source: "api", Message: "do it", Author: "alice", StartTime: time.Now(), Snapshot: runner.Snapshot()}
	rw, err := res.Wire()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.JobResult(context.Background(), s.ID, "x", rw); !errors.Is(err, ErrJobAuth) {
		t.Fatalf("bad token: %v", err)
	}
	dup, err := e.JobResult(context.Background(), s.ID, spec.Token, rw)
	if err != nil || dup {
		t.Fatalf("result dup %v err %v", dup, err)
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker still holds the session")
	}
	if snap := s.Snapshot(); snap.Status != agent.SessionStatusCompleted || snap.SuccessfulIterations != 1 {
		t.Errorf("status %s iterations %d", snap.Status, snap.SuccessfulIterations)
	}
	logs, _ := filepath.Glob(filepath.Join(e.config.LogsRoot, "*.md"))
	if len(logs) != 1 {
		t.Errorf("%d audit logs, want 1", len(logs))
	}

	if dup, err := e.JobResult(context.Background(), s.ID, spec.Token, rw); err != nil || !dup {
		t.Errorf("retried result dup %v err %v", dup, err)
	}
	logs, _ = filepath.Glob(filepath.Join(e.config.LogsRoot, "*.md"))
	if len(logs) != 1 {
		t.Errorf("a retried result was finalized again: %d audit logs", len(logs))
	}
	if _, err := e.JobSpec(s.ID, spec.Token); !errors.Is(err, ErrJobEnded) {
		t.Errorf("spec after the result: %v", err)
	}
	if _, err := e.JobReport(s.ID, spec.Token, &JobReport{Seq: 99, Session: w}); !errors.Is(err, ErrJobEnded) {
		t.Errorf("report after the result: %v", err)
	}
	if _, err := e.JobControl(context.Background(), s.ID, spec.Token, time.Second); !errors.Is(err, ErrJobEnded) {
		t.Errorf("control after the result: %v", err)
	}
	if err := e.queueSteer(s.ID, "late"); err == nil {
		t.Error("steer queued for an ended run")
	}
}

// The job secret is made once and read back, so tokens outlive a restart.
func TestLoadJobSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "job-secret")
	a, err := LoadJobSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadJobSecret(path)
	if err != nil || string(a) != string(b) {
		t.Fatalf("secret changed: err %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

// Exec events travel as deltas once the dispatcher has said where its copy
// ends: a delta from there is added; one from elsewhere (a restart, a lost
// report) leaves the events as they are, and the ack tells the runner where
// to send from. A report without events_from replaces them, as before.
func TestJobReportEventDeltas(t *testing.T) {
	env := setupTestEnv(t)
	e := env.handlers.Engine
	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "alice", "", 3, 60)
	spec, _ := startRemote(t, env, s)
	runner, _ := agent.NewSessionFromWire(mustWire(t, s), DecodeWire)
	at := time.Now()
	for i := 0; i < 3; i++ {
		runner.AppendExecEvent("tool_start", "step", at)
	}
	report := func(seq uint64, deltas bool, acked uint64) *JobReportAck {
		t.Helper()
		ack, err := e.JobReport(s.ID, spec.Token, withEventDelta(&JobReport{Seq: seq, Session: mustWire(t, runner)}, deltas, acked))
		if err != nil || ack.EventsSeq == nil {
			t.Fatalf("report %d: %+v %v", seq, ack, err)
		}
		return ack
	}
	seqs := func() []uint64 {
		var out []uint64
		for _, ev := range s.Snapshot().AgentEvents {
			out = append(out, ev.Seq)
		}
		return out
	}

	if ack := report(1, false, 0); *ack.EventsSeq != 3 || len(seqs()) != 3 {
		t.Fatalf("whole report: ack %d, events %v", *ack.EventsSeq, seqs())
	}
	runner.AppendExecEvent("tool_end", "step", at)
	runner.AppendExecEvent("tool_start", "step", at)
	rep := withEventDelta(&JobReport{Seq: 2, Session: mustWire(t, runner)}, true, 3)
	if rep.EventsFrom != 3 || len(rep.Session.AgentEvents) != 2 {
		t.Fatalf("the delta carries %d events from %d", len(rep.Session.AgentEvents), rep.EventsFrom)
	}
	if ack, err := e.JobReport(s.ID, spec.Token, rep); err != nil || *ack.EventsSeq != 5 {
		t.Fatalf("delta: %+v %v", ack, err)
	}
	if got := seqs(); len(got) != 5 || got[4] != 5 {
		t.Errorf("after the delta: %v", got)
	}
	// A delta from a point the dispatcher does not end at changes nothing.
	runner.AppendExecEvent("tool_end", "step", at)
	if ack := report(3, true, 2); *ack.EventsSeq != 5 || len(seqs()) != 5 {
		t.Errorf("mismatched delta: ack %d, events %v", *ack.EventsSeq, seqs())
	}
	// From where the ack said, it goes through.
	if ack := report(4, true, 5); *ack.EventsSeq != 6 || len(seqs()) != 6 {
		t.Errorf("resent delta: ack %d, events %v", *ack.EventsSeq, seqs())
	}
}
