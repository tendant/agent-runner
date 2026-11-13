package execution

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/agentsecrets"
)

// The dispatcher's watch over runs on other backends (JOBS_DESIGN.md §9,
// §10): a stopped run that does not end in its grace is ended; a runner not
// heard from for the heartbeat is asked after, and a gone one fails its
// session; a run a previous server started is taken back.

// recordStarted journals the run's handle, unless the run is already over
// (a fast result would otherwise leave a stale entry behind).
func (h *Engine) recordStarted(sessionID string, handle Handle) {
	v, ok := h.remote.Load(sessionID)
	if !ok {
		return
	}
	r := v.(*remoteRun)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		h.agentManager.RecordStarted(sessionID, handle.Backend, handle.Ref)
	}
}

// Reattach takes back a run of session that a previous server started on
// this backend and that is still going (§10). The session is the one
// rebuilt from the journal; the runner's next report brings it up to date,
// and its result is finalized here. Its runner is served at once; the run
// then holds a queue worker (Manager.Adopt), ahead of queued sessions, so
// it counts against AGENT_MAX_CONCURRENT.
func (h *Engine) Reattach(session *agent.Session, handle Handle) {
	done := h.runStarted(session.ID)
	h.registerRemote(session.ID)
	countRemoteStart(session)
	h.agentManager.Adopt(session, func(s *agent.Session) { h.superviseRemote(s, h.backend, handle, done) })
}

// superviseRemote returns when the run has ended, or when this server shuts
// down: the run then goes on, for the next server to re-attach to.
func (h *Engine) superviseRemote(session *agent.Session, b Backend, handle Handle, done <-chan struct{}) {
	heartbeat := h.config.Agent.JobHeartbeat
	if heartbeat <= 0 {
		heartbeat = 2 * time.Minute
	}
	grace := h.config.Agent.JobStopGrace
	if grace <= 0 {
		grace = 2 * time.Minute
	}
	every := heartbeat / 4 // the package's own min is for ints
	if every > 30*time.Second {
		every = 30 * time.Second
	}
	if every < 10*time.Millisecond {
		every = 10 * time.Millisecond
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	shutdown := h.agentManager.Context().Done()
	ctx := context.WithoutCancel(h.agentManager.Context())

	var stopBy time.Time // when a stopped run is ended, if still going
	killed := false
	// The session's own time limit, which its runner enforces; past it and
	// the grace, a runner that is alive but stuck is stopped from here.
	var overdueAt time.Time
	if snap := session.Snapshot(); snap.MaxTotalSeconds > 0 {
		overdueAt = snap.StartedAt.Add(time.Duration(snap.MaxTotalSeconds)*time.Second + grace)
	}
	for {
		select {
		case <-done:
			return
		case <-shutdown:
			slog.Info("server shutting down; the session's runner goes on", "session_id", session.ID, "backend", handle.Backend, "ref", handle.Ref)
			return
		case <-session.StopChan():
			if stopBy.IsZero() {
				stopBy = time.Now().Add(grace)
			}
		case <-tick.C:
		}

		if !overdueAt.IsZero() && time.Now().After(overdueAt) {
			overdueAt = time.Time{}
			h.markOverdue(session.ID)
			limit := session.Snapshot().MaxTotalSeconds
			slog.Warn("remote run past its time limit; stopping it", "session_id", session.ID, "max_total_seconds", limit)
			session.AddWarning(fmt.Sprintf("the run went on past its time limit (%ds): agent-runner stopped it", limit))
			session.RequestStop() // stopBy follows, then Stop after the grace
		}

		if !stopBy.IsZero() && !killed && time.Now().After(stopBy) {
			slog.Warn("stopped session's runner still going after its grace; ending it", "session_id", session.ID, "grace", grace)
			if err := b.Stop(ctx, handle); err != nil {
				slog.Warn("ending a runner failed", "session_id", session.ID, "error", err)
			}
			killed = true
		}

		if h.remoteSilentFor(session.ID) < heartbeat {
			continue
		}
		running, failure, err := b.Alive(ctx, handle)
		switch {
		case err != nil:
			slog.Warn("runner heartbeat: cannot tell whether it is alive", "session_id", session.ID, "error", err)
		case !running:
			if failure == "" {
				failure = "it is gone"
			}
			h.failRemote(session.ID, "the session's runner stopped without a result: "+failure)
		}
	}
}

// markOverdue records that a remote run went past its time limit: if it
// then ends without a result, it failed for that, not stopped as asked.
func (h *Engine) markOverdue(sessionID string) {
	if v, ok := h.remote.Load(sessionID); ok {
		r := v.(*remoteRun)
		r.mu.Lock()
		r.overdue = true
		r.mu.Unlock()
	}
}

// remoteSilentFor is how long since the runner of a session was last heard
// from.
func (h *Engine) remoteSilentFor(sessionID string) time.Duration {
	v, ok := h.remote.Load(sessionID)
	if !ok {
		return 0
	}
	r := v.(*remoteRun)
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastSeen)
}

// runnerExited is a backend's word that a session's runner process ended.
// Without a result that fails the session; with one (the runner exits after
// its result is taken) it is nothing. While this server shuts down the run
// is left for the next one.
func (h *Engine) runnerExited(sessionID, how string) {
	if h.agentManager.Context().Err() != nil {
		return
	}
	h.failRemote(sessionID, "the session's runner exited without a result ("+how+")")
}

// failRemote fails a remote run that has no result, and finalizes its
// session from the last state the runner reported, as a local run that
// failed is finalized. It reports whether it did: a run already finalized
// (its result taken) is left as it is.
func (h *Engine) failRemote(sessionID, reason string) bool {
	v, ok := h.remote.Load(sessionID)
	if !ok {
		return false
	}
	r := v.(*remoteRun)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	r.closed, r.ended = true, true
	overdue := r.overdue
	r.mu.Unlock()
	r.poke()
	defer h.runEnded(sessionID)

	slog.Error("remote run failed", "session_id", sessionID, "reason", reason)
	live, ok := h.agentManager.GetSessionDirect(sessionID)
	if !ok {
		return true
	}
	switch st := live.Snapshot().Status; {
	case st == agent.SessionStatusCompleted || st == agent.SessionStatusFailed || st == agent.SessionStatusStopped:
	case overdue:
		h.FailSession(sessionID, reason+"; it had run past its time limit")
	case live.StopRequested():
		// Asked to stop, and ended for not stopping: stopped, as asked.
		h.agentManager.MarkSessionStopped(sessionID, reason)
	default:
		h.FailSession(sessionID, reason)
	}
	h.agentManager.Ended(sessionID)
	snap := live.Snapshot()
	source := snap.Source
	if source == "" {
		source = "api"
	}
	// The run's own redactor went with it; the agent's secrets are the
	// same files here.
	_, secrets := h.agentEnv(sessionID, live)
	h.finalizeSession(&RunResult{
		SessionID: sessionID,
		Source:    source,
		Message:   snap.Message,
		Author:    snap.Author,
		StartTime: snap.StartedAt,
		Eval:      source == "eval",
		Snapshot:  snap,
		Redact:    agentsecrets.NewRedactor(secrets).Redact,
	})
	return true
}
