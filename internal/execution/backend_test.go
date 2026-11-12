package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// fakeBackend records what it was asked to start and leaves the run open
// until the test ends it.
type fakeBackend struct {
	started chan Spec
	err     error
}

func (b *fakeBackend) Name() string { return "fake" }
func (b *fakeBackend) Start(_ context.Context, spec Spec) (Handle, error) {
	if b.err != nil {
		return Handle{}, b.err
	}
	b.started <- spec
	return Handle{Backend: "fake", Ref: "1"}, nil
}
func (b *fakeBackend) Stop(context.Context, Handle) error { return nil }
func (b *fakeBackend) Alive(context.Context, Handle) (bool, string, error) {
	return true, "", nil
}

// A queued session runs on the local backend by default, as before: through
// the manager's worker to its finalized end, after which it is no longer
// running.
func TestQueuedSessionRunsOnLocalBackend(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.Engine.config.Agent.PlannerEnabled = false
	env.handlers.backend = &scriptedBackend{factory: func(int) *scriptedSession {
		return newScriptedSession([]string{"TASK: DONE"}, 0, 0)
	}}
	if got := env.handlers.Engine.backend.Name(); got != "local" {
		t.Fatalf("default backend %q", got)
	}
	s, err := env.handlers.agentManager.CreateSession("do it", nil, "test", "", 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.handlers.agentManager.Enqueue(s, env.handlers.RunQueued); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for s.Snapshot().Status != agent.SessionStatusCompleted {
		if time.Now().After(deadline) {
			t.Fatalf("status %s (%s)", s.Snapshot().Status, s.Snapshot().Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The run ends just after the status turns terminal (workspace cleanup).
	for env.handlers.running(s.ID) {
		if time.Now().After(deadline) {
			t.Fatal("run never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if alive, _, err := env.handlers.Engine.backend.Alive(context.Background(), Handle{Backend: "local", Ref: s.ID}); alive || err != nil {
		t.Errorf("alive %v err %v after the run", alive, err)
	}
}

// The worker holds the session until its run ends, not just until the
// backend has started it: that is what bounds concurrency for a backend
// whose runs are elsewhere.
func TestRunQueuedWaitsForTheRun(t *testing.T) {
	env := setupTestEnv(t)
	fb := &fakeBackend{started: make(chan Spec, 1)}
	env.handlers.SetBackend(fb)
	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "test", "", 3, 60)

	returned := make(chan struct{})
	go func() { env.handlers.RunQueued(s); close(returned) }()
	if spec := <-fb.started; spec.SessionID != s.ID {
		t.Fatalf("started %q", spec.SessionID)
	}
	select {
	case <-returned:
		t.Fatal("the worker let go of a running session")
	case <-time.After(50 * time.Millisecond):
	}
	if !env.handlers.running(s.ID) {
		t.Error("session not running")
	}
	env.handlers.runEnded(s.ID)
	env.handlers.runEnded(s.ID) // a second end is harmless
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not return after the run ended")
	}
}

// A run that cannot start fails its session and frees the worker.
func TestRunQueuedStartFailure(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.SetBackend(&fakeBackend{err: errors.New("no pods")})
	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "test", "", 3, 60)
	env.handlers.RunQueued(s)
	snap := s.Snapshot()
	if snap.Status != agent.SessionStatusFailed || !strings.Contains(snap.Error, "no pods") {
		t.Errorf("status %s error %q", snap.Status, snap.Error)
	}
	if env.handlers.running(s.ID) {
		t.Error("a run that never started is running")
	}
}
