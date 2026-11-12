package execution

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/logging"
)

// runnerBackend starts each session in a runner engine of its own in this
// process, talking to the dispatcher over HTTP: what the subprocess
// backend does, without the process.
type runnerBackend struct {
	t       *testing.T
	runner  *Engine
	errs    chan error
	started atomic.Int32
}

func (b *runnerBackend) Name() string { return "test-runner" }
func (b *runnerBackend) Start(ctx context.Context, spec Spec) (Handle, error) {
	b.started.Add(1)
	go func() {
		b.errs <- b.runner.RunRemote(context.Background(), &JobClient{URL: spec.ReportURL, SessionID: spec.SessionID, Token: spec.Token, RetryFor: 5 * time.Second})
	}()
	return Handle{Backend: "test-runner", Ref: spec.SessionID}, nil
}
func (b *runnerBackend) Stop(context.Context, Handle) error { return nil }
func (b *runnerBackend) Alive(context.Context, Handle) (bool, string, error) {
	return true, "", nil
}

type jobStack struct {
	dispatcher *engineTestEnv
	runner     *Engine
	backend    *runnerBackend
	exec       *scriptedBackend
}

// newJobStack is a dispatcher serving the job endpoints and a runner engine
// that shares its configuration (as run-session shares DATA_DIR), each with
// its own session manager.
func newJobStack(t *testing.T, factory func(int) *scriptedSession) *jobStack {
	t.Helper()
	env := setupTestEnv(t)
	cfg := env.handlers.Engine.config
	cfg.Agent.PlannerEnabled = false
	sb := &scriptedBackend{factory: factory}

	mgr := agent.NewManager(3600, 10, 1)
	t.Cleanup(mgr.Stop)
	shim := &engineShim{executor: env.handlers.executor, backend: sb, agentManager: mgr}
	shim.Engine = New(cfg, mgr, executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds), logging.NewRunLogger(cfg.LogsRoot), shim)
	shim.Engine.EnableRunnerMode(false)

	ts := httptest.NewServer(env.handlers.Engine.JobHandler())
	t.Cleanup(ts.Close)
	env.handlers.SetJobDispatcherURL(ts.URL)
	rb := &runnerBackend{t: t, runner: shim.Engine, errs: make(chan error, 4)}
	env.handlers.SetBackend(rb)
	return &jobStack{dispatcher: env, runner: shim.Engine, backend: rb, exec: sb}
}

func (js *jobStack) start(t *testing.T, msg string) *agent.Session {
	t.Helper()
	s, err := js.dispatcher.handlers.agentManager.CreateSession(msg, nil, "alice", "", 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.dispatcher.handlers.agentManager.Enqueue(s, js.dispatcher.handlers.RunQueued); err != nil {
		t.Fatal(err)
	}
	return s
}

// waitRunner waits for the runner to return and gives its error.
func (js *jobStack) waitRunner(t *testing.T) error {
	t.Helper()
	select {
	case err := <-js.backend.errs:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the runner never returned")
	}
	return nil
}

func waitStatus(t *testing.T, s *agent.Session, want agent.SessionStatus) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for s.Snapshot().Status != want {
		if time.Now().After(deadline) {
			t.Fatalf("status %s (%s), want %s", s.Snapshot().Status, s.Snapshot().Error, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session runs in the runner and ends finalized by the dispatcher: its
// state reaches the dispatcher's session, the audit log is written there,
// the worker is let go, and the trace is one.
func TestRunRemoteCompletes(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		tp.Shutdown(context.Background())
	})

	js := newJobStack(t, func(int) *scriptedSession {
		return newScriptedSession([]string{"TASK: DONE"}, 0, 0)
	})
	s := js.start(t, "do it")
	if err := js.waitRunner(t); err != nil {
		t.Fatalf("runner: %v", err)
	}
	waitStatus(t, s, agent.SessionStatusCompleted)
	snap := s.Snapshot()
	if snap.SuccessfulIterations != 1 || len(snap.Iterations) != 1 {
		t.Errorf("iterations %d/%d", snap.SuccessfulIterations, len(snap.Iterations))
	}
	logs, _ := filepath.Glob(filepath.Join(js.dispatcher.handlers.Engine.config.LogsRoot, "*.md"))
	if len(logs) != 1 {
		t.Errorf("%d audit logs, want 1 (the dispatcher's)", len(logs))
	}
	deadline := time.Now().Add(5 * time.Second)
	for js.dispatcher.handlers.running(s.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the dispatcher's worker still holds the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := js.backend.started.Load(); got != 1 {
		t.Errorf("started %d times", got)
	}

	tp.ForceFlush(context.Background())
	var root, fin sdktrace.ReadOnlySpan
	for _, sp := range rec.Ended() {
		switch sp.Name() {
		case "agent.session":
			root = sp
		case "agent.dispatch.finalize":
			fin = sp
		}
	}
	if root == nil || fin == nil {
		t.Fatalf("spans: session %v, dispatch.finalize %v", root != nil, fin != nil)
	}
	if fin.SpanContext().TraceID() != root.SpanContext().TraceID() {
		t.Error("the dispatcher's finalization is in another trace")
	}
	if snap.TraceID != root.SpanContext().TraceID().String() {
		t.Errorf("session trace id %q, root %s", snap.TraceID, root.SpanContext().TraceID())
	}
}

// A stop asked of the dispatcher reaches the runner's run, which stops and
// reports it; a steer message reaches the runner's live CLI session.
func TestRunRemoteStopAndSteer(t *testing.T) {
	var live *scriptedSession
	ready := make(chan struct{})
	js := newJobStack(t, func(int) *scriptedSession {
		live = newScriptedSession([]string{"working"}, 0, 1) // blocks until aborted
		live.steerable, live.steered = true, make(chan string, 1)
		close(ready)
		return live
	})
	s := js.start(t, "long job")
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the runner never started the CLI")
	}
	// The runner's own copy runs; steer it through the dispatcher's Steer,
	// as a chat message would, once the runner has said its CLI takes it.
	deadline := time.Now().Add(30 * time.Second)
	for js.dispatcher.handlers.Steer(s.ID, "use main") != nil {
		if time.Now().After(deadline) {
			t.Fatal("the dispatcher never took a steer for the remote run")
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case text := <-live.steered:
		if text != "use main" {
			t.Fatalf("steered %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("steer never reached the runner")
	}
	if err := js.dispatcher.handlers.agentManager.StopSession(s.ID); err != nil {
		t.Fatal(err)
	}
	if err := js.waitRunner(t); err != nil {
		t.Fatalf("runner: %v", err)
	}
	waitStatus(t, s, agent.SessionStatusStopped)
}

// A runner whose run the dispatcher has ended stops and exits.
func TestRunRemoteEndedByDispatcher(t *testing.T) {
	js := newJobStack(t, func(int) *scriptedSession {
		return newScriptedSession([]string{"working"}, 0, 1) // blocks
	})
	s := js.start(t, "long job")
	deadline := time.Now().Add(30 * time.Second)
	for js.exec.startCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the runner never started the CLI")
		}
		time.Sleep(10 * time.Millisecond)
	}
	js.dispatcher.handlers.endRemote(s.ID) // e.g. the heartbeat gave up on it
	err := js.waitRunner(t)
	if !errors.Is(err, ErrJobEnded) {
		t.Errorf("runner error %v, want ErrJobEnded", err)
	}
}

// Requests that cannot reach the dispatcher are retried; refusals are not.
func TestJobClientRetries(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJobJSON(w, http.StatusOK, map[string]any{"session_id": "agent-1", "message": "hi"})
	}))
	defer ts.Close()
	c := &JobClient{URL: ts.URL, SessionID: "agent-1", Token: "tok", RetryFor: time.Minute}
	w, err := c.Spec(context.Background())
	if err != nil || w.ID != "agent-1" || calls.Load() != 3 {
		t.Fatalf("spec %+v err %v after %d calls", w, err, calls.Load())
	}
	bad := &JobClient{URL: ts.URL, SessionID: "agent-1", Token: "x", RetryFor: time.Minute}
	if _, err := bad.Spec(context.Background()); !errors.Is(err, ErrJobAuth) {
		t.Errorf("bad token: %v", err)
	}
}

// A runner sends back _send/ files up to the cap, and proposals without a
// secret; the rest are named in warnings.
func TestRunnerCapsAndFilters(t *testing.T) {
	snap := &agent.SessionWire{OutputFiles: []agent.OutputFileWire{
		{OutputFile: agent.OutputFile{Name: "a"}, Data: make([]byte, 60)},
		{OutputFile: agent.OutputFile{Name: "b"}, Data: make([]byte, 60)},
		{OutputFile: agent.OutputFile{Name: "c"}, Data: make([]byte, 30)},
	}}
	capOutputs(snap, 100)
	if len(snap.OutputFiles[0].Data) != 60 || snap.OutputFiles[1].Data != nil || len(snap.OutputFiles[2].Data) != 30 {
		t.Errorf("sizes %d %d %d", len(snap.OutputFiles[0].Data), len(snap.OutputFiles[1].Data), len(snap.OutputFiles[2].Data))
	}
	if len(snap.Warnings) != 1 || !strings.Contains(snap.Warnings[0], "_send/b") {
		t.Errorf("warnings %v", snap.Warnings)
	}
}
