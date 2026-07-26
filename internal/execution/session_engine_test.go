package execution

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
)

// scriptedSession is a fake persistent session with per-prompt behavior.
type scriptedSession struct {
	mu      sync.Mutex
	prompts []executor.PromptRequest
	outputs []string // response per prompt; last entry repeats
	dieOn   int      // 1-based prompt index returning ErrSessionDead; 0 = never
	blockOn int      // 1-based prompt index that blocks until Abort; 0 = never

	events    chan executor.Event
	abortCh   chan struct{}
	abortOnce sync.Once
	closeOnce sync.Once
}

func newScriptedSession(outputs []string, dieOn, blockOn int) *scriptedSession {
	return &scriptedSession{
		outputs: outputs,
		dieOn:   dieOn,
		blockOn: blockOn,
		events:  make(chan executor.Event, 8),
		abortCh: make(chan struct{}),
	}
}

func (s *scriptedSession) Prompt(ctx context.Context, req executor.PromptRequest) (*executor.ExecutionResult, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, req)
	n := len(s.prompts)
	s.mu.Unlock()

	if s.blockOn == n {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.abortCh:
			return nil, context.Canceled
		}
	}
	if s.dieOn == n {
		return &executor.ExecutionResult{}, executor.ErrSessionDead
	}
	out := ""
	if len(s.outputs) > 0 {
		idx := n - 1
		if idx >= len(s.outputs) {
			idx = len(s.outputs) - 1
		}
		out = s.outputs[idx]
	}
	return &executor.ExecutionResult{Output: out}, nil
}

func (s *scriptedSession) promptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

func (s *scriptedSession) prompt(i int) executor.PromptRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompts[i]
}

func (s *scriptedSession) Steer(string) error    { return executor.ErrUnsupported }
func (s *scriptedSession) FollowUp(string) error { return executor.ErrUnsupported }
func (s *scriptedSession) Abort() error {
	s.abortOnce.Do(func() { close(s.abortCh) })
	return nil
}
func (s *scriptedSession) Events() <-chan executor.Event { return s.events }
func (s *scriptedSession) Close() error {
	s.closeOnce.Do(func() { close(s.events) })
	return nil
}

// scriptedBackend hands out scripted sessions and counts Starts.
type scriptedBackend struct {
	mu       sync.Mutex
	factory  func(startCount int) *scriptedSession
	sessions []*scriptedSession
}

func (b *scriptedBackend) Start(_ context.Context, _ string, _ executor.SessionOptions) (executor.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.factory(len(b.sessions) + 1)
	b.sessions = append(b.sessions, s)
	return s, nil
}

func (b *scriptedBackend) Persistent() bool { return true }

func (b *scriptedBackend) session(i int) *scriptedSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[i]
}

func (b *scriptedBackend) startCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions)
}

// A persistent backend gets one session for the whole run: full prompt on
// iteration 1, incremental continuation on iteration 2.
func TestPersistentSession_IncrementalPrompts(t *testing.T) {
	env := setupTestEnv(t)
	// These tests exercise the iteration loop; the planner needs a real
	// LLM client, so it stays off (shim's PlannerClient is nil).
	env.handlers.Engine.config.Agent.PlannerEnabled = false
	sb := &scriptedBackend{factory: func(int) *scriptedSession {
		return newScriptedSession([]string{"working on it", "TASK: DONE"}, 0, 0)
	}}
	env.handlers.backend = sb

	session, err := env.handlers.agentManager.CreateSession("do something", nil, "test", "", 5, 60)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	env.handlers.ExecuteAgent(session)

	snap := session.Snapshot()
	if snap.Status != agent.SessionStatusCompleted {
		t.Fatalf("status = %s (err: %s), want completed", snap.Status, snap.Error)
	}
	if sb.startCount() != 1 {
		t.Fatalf("backend started %d sessions, want 1", sb.startCount())
	}
	sess := sb.session(0)
	if sess.promptCount() != 2 {
		t.Fatalf("prompts = %d, want 2", sess.promptCount())
	}
	first, second := sess.prompt(0), sess.prompt(1)
	if first.SystemPrompt == "" || first.Message != "do something" {
		t.Errorf("first prompt should be full: sys=%d chars, msg=%q", len(first.SystemPrompt), first.Message)
	}
	if second.SystemPrompt != "" || !strings.Contains(second.Message, "Continue working") {
		t.Errorf("second prompt should be incremental: sys=%q msg=%q", second.SystemPrompt, second.Message)
	}
}

// A dead session process is restarted once, and the first prompt on the new
// session rebuilds full context.
func TestPersistentSession_RestartOnDeath(t *testing.T) {
	env := setupTestEnv(t)
	// These tests exercise the iteration loop; the planner needs a real
	// LLM client, so it stays off (shim's PlannerClient is nil).
	env.handlers.Engine.config.Agent.PlannerEnabled = false
	sb := &scriptedBackend{factory: func(n int) *scriptedSession {
		if n == 1 {
			return newScriptedSession(nil, 1, 0) // dies on its first prompt
		}
		return newScriptedSession([]string{"TASK: DONE"}, 0, 0)
	}}
	env.handlers.backend = sb

	session, err := env.handlers.agentManager.CreateSession("do something", nil, "test", "", 5, 60)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	env.handlers.ExecuteAgent(session)

	snap := session.Snapshot()
	if snap.Status != agent.SessionStatusCompleted {
		t.Fatalf("status = %s (err: %s), want completed", snap.Status, snap.Error)
	}
	if sb.startCount() != 2 {
		t.Fatalf("backend started %d sessions, want 2 (restart after death)", sb.startCount())
	}
	if replacement := sb.session(1).prompt(0); replacement.SystemPrompt == "" {
		t.Error("first prompt after restart should rebuild the full system prompt")
	}
	found := false
	for _, w := range snap.Warnings {
		if strings.Contains(w, "restarted") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected restart warning, got %v", snap.Warnings)
	}
}

// A user stop aborts the in-flight prompt live instead of waiting the
// iteration out.
func TestPersistentSession_LiveAbortOnStop(t *testing.T) {
	env := setupTestEnv(t)
	// These tests exercise the iteration loop; the planner needs a real
	// LLM client, so it stays off (shim's PlannerClient is nil).
	env.handlers.Engine.config.Agent.PlannerEnabled = false
	sb := &scriptedBackend{factory: func(int) *scriptedSession {
		return newScriptedSession(nil, 0, 1) // first prompt blocks until Abort
	}}
	env.handlers.backend = sb

	session, err := env.handlers.agentManager.CreateSession("do something", nil, "test", "", 5, 300)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	done := make(chan struct{})
	go func() {
		env.handlers.ExecuteAgent(session)
		close(done)
	}()

	// Wait until the prompt is in flight, then request a stop.
	deadline := time.After(10 * time.Second)
	for sb.startCount() == 0 || sb.session(0).promptCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("prompt never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	session.RequestStop()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run did not finish after stop — live abort failed")
	}
	snap := session.Snapshot()
	if snap.Status != agent.SessionStatusStopped {
		t.Errorf("status = %s, want stopped", snap.Status)
	}
}
