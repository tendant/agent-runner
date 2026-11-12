package execution

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// Spec is what a backend needs to start a session's run (JOBS_DESIGN.md §6).
// A runner in another process fetches the session itself from ReportURL
// (§7.1), authenticated by Token (§11); the local backend needs neither.
type Spec struct {
	SessionID string
	ReportURL string
	Token     string
}

// Handle names a started run: its backend and that backend's reference to
// it (the session ID for local, a pid, a Job name). Recovery (§10) reads it
// back.
type Handle struct {
	Backend string `json:"backend"`
	Ref     string `json:"ref,omitempty"`
}

// Backend runs sessions somewhere: in process (local), as a child process
// (subprocess) or as a Kubernetes Job (k8s-job).
type Backend interface {
	Name() string
	// Start starts the session's run and returns without waiting for it. The
	// run ends with its result finalized, after which the backend calls the
	// engine's runEnded for the session.
	Start(ctx context.Context, spec Spec) (Handle, error)
	// Stop ends a run that did not stop when asked within its grace (§9).
	Stop(ctx context.Context, h Handle) error
	// Alive reports whether the run still exists and, if it ended in
	// failure, why; for the heartbeat check and recovery (§10).
	Alive(ctx context.Context, h Handle) (running bool, failure string, err error)
}

// SetBackend sets where sessions run. Call before any session runs; the
// default is the local backend.
func (h *Engine) SetBackend(b Backend) { h.backend = b }

// RunQueued is the manager's worker function for a session: it starts the
// session's run on the backend and returns once the run has ended, so a
// worker holds one session at a time whatever the backend.
func (h *Engine) RunQueued(session *agent.Session) {
	done := h.runStarted(session.ID)
	b := h.backend
	if _, err := b.Start(h.agentManager.Context(), Spec{SessionID: session.ID}); err != nil {
		slog.Error("execution backend failed to start session", "backend", b.Name(), "session_id", session.ID, "error", err)
		h.FailSession(session.ID, fmt.Sprintf("could not start the run (%s): %v", b.Name(), err))
		h.runEnded(session.ID)
	}
	<-done
}

// runStarted registers a session's run and returns the channel runEnded
// closes.
func (h *Engine) runStarted(id string) <-chan struct{} {
	done := make(chan struct{})
	h.runs.Store(id, done)
	return done
}

// runEnded marks a session's run over. Safe to call more than once.
func (h *Engine) runEnded(id string) {
	if v, ok := h.runs.LoadAndDelete(id); ok {
		close(v.(chan struct{}))
	}
}

// running reports whether a session's run has started and not yet ended.
func (h *Engine) running(id string) bool {
	_, ok := h.runs.Load(id)
	return ok
}

// localBackend runs sessions in this process: today's behavior, with no
// HTTP between the run and its finalization.
type localBackend struct{ h *Engine }

func (b *localBackend) Name() string { return "local" }

func (b *localBackend) Start(ctx context.Context, spec Spec) (Handle, error) {
	session, ok := b.h.agentManager.GetSessionDirect(spec.SessionID)
	if !ok {
		return Handle{}, fmt.Errorf("session %s not found", spec.SessionID)
	}
	go func() {
		defer b.h.runEnded(spec.SessionID)
		b.h.ExecuteAgentWithContext(ctx, session)
	}()
	return Handle{Backend: "local", Ref: spec.SessionID}, nil
}

// Stop has nothing to end: an in-process run honors the stop request itself
// (Session.RequestStop), and there is no separate job to delete.
func (b *localBackend) Stop(context.Context, Handle) error { return nil }

// Alive asks the engine. A local run cannot outlive its process, so after a
// restart every local run reads as gone.
func (b *localBackend) Alive(_ context.Context, h Handle) (bool, string, error) {
	return b.h.running(h.Ref), "", nil
}
