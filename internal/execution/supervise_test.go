package execution

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// lifeBackend is a remote backend whose run is alive until the test says
// otherwise, and which counts Stop calls.
type lifeBackend struct {
	started chan Spec
	alive   atomic.Bool
	stops   atomic.Int32
}

func (b *lifeBackend) Name() string { return "life" }
func (b *lifeBackend) Start(_ context.Context, spec Spec) (Handle, error) {
	b.alive.Store(true)
	b.started <- spec
	return Handle{Backend: "life", Ref: "7", Session: spec.SessionID}, nil
}
func (b *lifeBackend) Stop(context.Context, Handle) error { b.stops.Add(1); return nil }
func (b *lifeBackend) Alive(context.Context, Handle) (bool, string, error) {
	if b.alive.Load() {
		return true, "", nil
	}
	return false, "OOMKilled", nil
}

func startLife(t *testing.T, env *engineTestEnv, msg string) (*lifeBackend, *agent.Session, Spec, <-chan struct{}) {
	t.Helper()
	b := &lifeBackend{started: make(chan Spec, 1)}
	env.handlers.SetBackend(b)
	env.handlers.SetJobDispatcherURL("http://127.0.0.1:1")
	s, _ := env.handlers.agentManager.CreateSession(msg, nil, "alice", "", 3, 60)
	returned := make(chan struct{})
	if err := env.handlers.agentManager.Enqueue(s, func(s *agent.Session) { env.handlers.RunQueued(s); close(returned) }); err != nil {
		t.Fatal(err)
	}
	select {
	case spec := <-b.started:
		return b, s, spec, returned
	case <-time.After(5 * time.Second):
		t.Fatal("backend never started")
	}
	return nil, nil, Spec{}, nil
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal(what)
	}
}

// A runner that goes silent is asked after once the heartbeat passes; a
// gone one fails its session with the backend's reason, the session is
// finalized (its audit log written) and the worker let go.
func TestHeartbeatFailsAGoneRunner(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.Engine.config.Agent.JobHeartbeat = 100 * time.Millisecond
	b, s, spec, returned := startLife(t, env, "long job")
	e := env.handlers.Engine

	// Alive, so silence alone fails nothing.
	time.Sleep(300 * time.Millisecond)
	if st := s.Snapshot().Status; st != agent.SessionStatusRunning {
		t.Fatalf("an alive runner's session is %s", st)
	}
	b.alive.Store(false)
	waitClosed(t, returned, "the worker still holds a gone runner's session")
	snap := s.Snapshot()
	if snap.Status != agent.SessionStatusFailed || !strings.Contains(snap.Error, "OOMKilled") {
		t.Errorf("status %s error %q", snap.Status, snap.Error)
	}
	if ents, _ := os.ReadDir(e.config.LogsRoot); len(ents) == 0 {
		t.Error("no audit log for the failed run")
	}
	if _, err := e.JobSpec(s.ID, spec.Token); err == nil {
		t.Error("the failed run's runner is still served")
	}
	// A late result from it is refused, not finalized twice.
	w, _ := (&RunResult{SessionID: s.ID, Snapshot: s.Snapshot()}).Wire()
	if _, err := e.JobResult(context.Background(), s.ID, spec.Token, w); err == nil {
		t.Error("a result after the run failed was taken")
	}
}

// A runner process that exits without a result fails its session; one that
// exits after its result changes nothing.
func TestRunnerExited(t *testing.T) {
	env := setupTestEnv(t)
	e := env.handlers.Engine
	_, s, _, returned := startLife(t, env, "job")
	e.runnerExited(s.ID, "signal: killed")
	waitClosed(t, returned, "worker not let go")
	if snap := s.Snapshot(); snap.Status != agent.SessionStatusFailed || !strings.Contains(snap.Error, "signal: killed") {
		t.Errorf("status %s error %q", snap.Status, snap.Error)
	}

	_, s2, spec2, returned2 := startLife(t, env, "job 2")
	runner, _ := agent.NewSessionFromWire(mustWire(t, s2), DecodeWire)
	runner.Complete()
	w, _ := (&RunResult{SessionID: s2.ID, Source: "api", Message: "job 2", StartTime: time.Now(), Snapshot: runner.Snapshot()}).Wire()
	if _, err := e.JobResult(context.Background(), s2.ID, spec2.Token, w); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, returned2, "worker not let go after the result")
	e.runnerExited(s2.ID, "exit status 0")
	if st := s2.Snapshot().Status; st != agent.SessionStatusCompleted {
		t.Errorf("a delivered result was undone: %s", st)
	}
}

// A stopped run that is still going after its grace is ended through the
// backend; when its runner then goes, the session is stopped, as asked.
func TestStopGraceEndsTheRunner(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.Engine.config.Agent.JobStopGrace = 50 * time.Millisecond
	env.handlers.Engine.config.Agent.JobHeartbeat = 40 * time.Millisecond
	b, s, _, returned := startLife(t, env, "job")
	_ = env.handlers.agentManager.StopSession(s.ID)
	deadline := time.Now().Add(5 * time.Second)
	for b.stops.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the backend was never asked to end the run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.alive.Store(false)
	waitClosed(t, returned, "worker not let go")
	if st := s.Snapshot().Status; st != agent.SessionStatusStopped {
		t.Errorf("status %s", st)
	}
	if n := b.stops.Load(); n != 1 {
		t.Errorf("Stop called %d times", n)
	}
}

// Shutting down leaves a remote run alone: the worker returns, the session
// is not failed, and the runner is not ended, for the next server.
func TestShutdownLeavesRemoteRuns(t *testing.T) {
	env := setupTestEnv(t)
	b, s, _, returned := startLife(t, env, "job")
	env.handlers.agentManager.Stop()
	waitClosed(t, returned, "the worker kept waiting through shutdown")
	if st := s.Snapshot().Status; st != agent.SessionStatusRunning {
		t.Errorf("status %s after shutdown", st)
	}
	env.handlers.Engine.runnerExited(s.ID, "exit status 1") // the old server sees its child go
	if st := s.Snapshot().Status; st != agent.SessionStatusRunning {
		t.Errorf("status %s: a shutting-down server finalized the run", st)
	}
	if b.stops.Load() != 0 {
		t.Error("the runner was ended")
	}
}

// A run taken back after a restart is served again under the same token,
// and its result is finalized here.
func TestReattach(t *testing.T) {
	env := setupTestEnv(t)
	e := env.handlers.Engine
	b := &lifeBackend{started: make(chan Spec, 1)}
	b.alive.Store(true)
	e.SetBackend(b)
	s := &agent.Session{ID: "agent-restored", Message: "job", Status: agent.SessionStatusRunning, StartedAt: time.Now()}
	if err := env.handlers.agentManager.RestoreSession(s); err != nil {
		t.Fatal(err)
	}
	e.Reattach(s, Handle{Backend: "life", Ref: "7", Session: s.ID})
	token := e.SessionToken(s.ID)
	w := mustWire(t, s)
	w.CurrentIteration = 2
	if err := e.JobReport(s.ID, token, &JobReport{Seq: 9, Session: w}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().CurrentIteration != 2 {
		t.Error("report not applied")
	}
	runner, _ := agent.NewSessionFromWire(w, DecodeWire)
	runner.Complete()
	rw, _ := (&RunResult{SessionID: s.ID, Source: "api", Message: "job", StartTime: time.Now(), Snapshot: runner.Snapshot()}).Wire()
	if _, err := e.JobResult(context.Background(), s.ID, token, rw); err != nil {
		t.Fatal(err)
	}
	if st := s.Snapshot().Status; st != agent.SessionStatusCompleted {
		t.Errorf("status %s", st)
	}
	if e.running(s.ID) {
		t.Error("the re-attached run is still open")
	}
}

func mustWire(t *testing.T, s *agent.Session) *agent.SessionWire {
	t.Helper()
	w, err := s.Wire()
	if err != nil {
		t.Fatal(err)
	}
	return w
}
