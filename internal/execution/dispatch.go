package execution

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/metrics"
	"github.com/agent-runner/agent-runner/internal/proposals"
	"github.com/agent-runner/agent-runner/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
)

// The dispatcher's half of the job protocol (JOBS_DESIGN.md §7): what the
// /internal/sessions/{id}/* endpoints call for a session that runs on a
// backend other than local.

var (
	// ErrJobAuth: no remote run of that session, or the wrong token. The two
	// are not told apart, so a token says nothing about other sessions.
	ErrJobAuth = errors.New("unknown session or bad token")
	// ErrJobEnded: the run has ended (its result was taken, or it was failed
	// without one); the runner has nothing left to do.
	ErrJobEnded = errors.New("session run has ended")
)

// MaxControlWait caps a control long poll.
const MaxControlWait = 60 * time.Second

// JobReport is a runner's progress report (§7.2): the session's state, with
// a sequence number so a retried or reordered report is applied once and
// never over a newer one.
type JobReport struct {
	Seq     uint64             `json:"seq"`
	Session *agent.SessionWire `json:"session"`
	// Steerable: the run's CLI session takes steer messages now (a
	// persistent backend, between its start and end), so the dispatcher's
	// Steer can queue them instead of failing (§7.3).
	Steerable bool `json:"steerable,omitempty"`
	// EventsFrom > 0: Session.AgentEvents holds only the exec events after
	// that Seq, the last one the dispatcher acknowledged (§7.4); 0: all.
	EventsFrom uint64 `json:"events_from,omitempty"`
}

// JobReportAck answers a report. EventsSeq is the newest exec event the
// dispatcher holds: the runner sends the events after it next time. A
// dispatcher that does not send it takes no deltas, only whole reports.
type JobReportAck struct {
	Seq       uint64  `json:"seq"`
	EventsSeq *uint64 `json:"events_seq,omitempty"`
}

// JobControl answers a runner's control poll (§7.3).
type JobControl struct {
	Stop  bool     `json:"stop"`
	Steer []string `json:"steer,omitempty"`
}

// remoteRun is the dispatcher's record of a session running elsewhere.
type remoteRun struct {
	mu        sync.Mutex
	seq       uint64    // last report applied
	ended     bool      // the runner has nothing left to do: its requests get ErrJobEnded
	result    bool      // the result was taken
	closed    bool      // finalized here: the result taken, or the run failed without one
	steerable bool      // as of the last report
	overdue   bool      // went past its time limit and was stopped from here
	lastSeen  time.Time // the runner's last request, for the heartbeat (§9)
	steer     []string  // steer messages for the next control poll
	wake      chan struct{}
}

func (r *remoteRun) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// SetJobSecret sets the key session tokens are derived from. A dispatcher
// keeps it across restarts (STATE_ROOT/job-secret) so a running job's token
// stays valid (§10); New starts with a random one.
func (h *Engine) SetJobSecret(secret []byte) { h.jobSecret = secret }

// LoadJobSecret reads the job secret at path, creating it (0600) the first
// time.
func LoadJobSecret(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	b := randomSecret()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

func randomSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("job secret: %v", err))
	}
	return b
}

// SessionToken is the bearer token of a session's runner (§11): an HMAC of
// the session ID, so the dispatcher can check it after a restart without
// having kept it.
func (h *Engine) SessionToken(sessionID string) string {
	m := hmac.New(sha256.New, h.jobSecret)
	m.Write([]byte("agent-runner session " + sessionID))
	return hex.EncodeToString(m.Sum(nil))
}

// registerRemote records that a session is about to run elsewhere, so its
// runner's requests are accepted. Runs of sessions the manager no longer
// holds are dropped here.
func (h *Engine) registerRemote(sessionID string) {
	h.remote.Range(func(k, _ any) bool {
		if _, ok := h.agentManager.GetSessionDirect(k.(string)); !ok {
			h.remote.Delete(k)
		}
		return true
	})
	h.remote.Store(sessionID, &remoteRun{wake: make(chan struct{}, 1), lastSeen: time.Now()})
}

// endRemote marks a remote run over: the runner's further requests get
// ErrJobEnded.
func (h *Engine) endRemote(sessionID string) {
	if v, ok := h.remote.Load(sessionID); ok {
		r := v.(*remoteRun)
		r.mu.Lock()
		r.ended = true
		r.mu.Unlock()
		r.poke()
	}
}

func (h *Engine) authJob(sessionID, token string) (*agent.Session, *remoteRun, error) {
	v, ok := h.remote.Load(sessionID)
	if !ok || !hmac.Equal([]byte(token), []byte(h.SessionToken(sessionID))) {
		return nil, nil, ErrJobAuth
	}
	s, ok := h.agentManager.GetSessionDirect(sessionID)
	if !ok {
		return nil, nil, ErrJobAuth
	}
	r := v.(*remoteRun)
	r.mu.Lock()
	r.lastSeen = time.Now()
	r.mu.Unlock()
	return s, r, nil
}

// JobSpec is the session as its runner starts it (§7.1).
func (h *Engine) JobSpec(sessionID, token string) (*agent.SessionWire, error) {
	s, run, err := h.authJob(sessionID, token)
	if err != nil {
		return nil, err
	}
	run.mu.Lock()
	ended := run.ended
	run.mu.Unlock()
	if ended {
		return nil, ErrJobEnded
	}
	return s.Wire()
}

// JobReport applies a runner's report to the session, unless a report as
// new was already applied (then it is a no-op).
func (h *Engine) JobReport(sessionID, token string, rep *JobReport) (*JobReportAck, error) {
	s, run, err := h.authJob(sessionID, token)
	if err != nil {
		return nil, err
	}
	if rep.Session == nil {
		return nil, fmt.Errorf("report without a session")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.ended {
		return nil, ErrJobEnded
	}
	have := agent.LastExecSeq(s.Snapshot().AgentEvents)
	if rep.Seq <= run.seq {
		return &JobReportAck{Seq: rep.Seq, EventsSeq: &have}, nil
	}
	// The exec events: all of them (events_from 0), or only the new ones,
	// added to those held when they start where those end. A delta from
	// elsewhere (this dispatcher restarted, a report was lost) leaves the
	// events as they are; the ack says where they end, and the runner
	// sends from there.
	w := *rep.Session
	if rep.EventsFrom > 0 {
		cur := s.Snapshot().AgentEvents
		if rep.EventsFrom == have {
			w.AgentEvents = agent.MergeExecEvents(cur, rep.Session.AgentEvents)
		} else {
			w.AgentEvents, w.ExecEventSeq = cur, have
		}
	}
	if err := s.ApplyWire(&w, DecodeWire); err != nil {
		return nil, err
	}
	run.seq, run.steerable = rep.Seq, rep.Steerable
	have = agent.LastExecSeq(w.AgentEvents)
	return &JobReportAck{Seq: rep.Seq, EventsSeq: &have}, nil
}

// JobResult takes a runner's result once: what the runner left to the
// dispatcher (outputs, proposals, schedules: §8.4) is done here, then the
// session gets its final state and is finalized, as a local run is, and the
// run ends. A repeat of a taken result reports duplicate (the runner's retry
// after a lost reply). ctx carries the runner's span, which the dispatcher's
// part of the trace joins.
func (h *Engine) JobResult(ctx context.Context, sessionID, token string, w *RunResultWire) (duplicate bool, err error) {
	s, run, err := h.authJob(sessionID, token)
	if err != nil {
		return false, err
	}
	if w.SessionID != sessionID {
		return false, fmt.Errorf("result for %s sent to %s", w.SessionID, sessionID)
	}
	if _, err := w.RunResult(); err != nil { // check it before taking it
		return false, err
	}
	run.mu.Lock()
	if run.result {
		run.mu.Unlock()
		return true, nil
	}
	if run.ended {
		run.mu.Unlock()
		return false, ErrJobEnded
	}
	run.result, run.ended, run.closed = true, true, true
	run.mu.Unlock()
	run.poke()

	ctx, span := tracing.Start(context.WithoutCancel(ctx), "agent.dispatch.finalize", attribute.String("session.id", sessionID))
	defer span.End()
	// Notes and warnings from the deferred work go on the final state
	// before the session turns terminal, as in a local run.
	h.finishDeferred(ctx, sessionID, w)
	res, err := w.RunResult()
	if err != nil {
		slog.Error("job result: rebuilding it", "session_id", sessionID, "error", err)
		return false, err
	}
	if err := s.ApplyWire(w.Snapshot, DecodeWire); err != nil {
		slog.Error("job result: applying the final state", "session_id", sessionID, "error", err)
	}
	h.agentManager.Ended(sessionID)
	h.finalizeSession(res)
	h.runEnded(sessionID)
	return false, nil
}

// finishDeferred does what a runner leaves to the dispatcher (§8.4):
// persisting the _send/ files, submitting proposals and schedule entries.
// What it reports goes on w's snapshot.
func (h *Engine) finishDeferred(ctx context.Context, sessionID string, w *RunResultWire) {
	snap := w.Snapshot
	if h.config.OutputsRoot != "" && len(snap.OutputFiles) > 0 {
		files := make([]agent.OutputFile, len(snap.OutputFiles))
		for i, f := range snap.OutputFiles {
			files[i] = f.OutputFile
			files[i].Data = f.Data
		}
		if err := persistOutputFiles(files, h.config.OutputsRoot, sessionID); err != nil {
			slog.Warn("failed to persist output files", "session_id", sessionID, "error", err)
		}
	}
	if len(w.Proposals) > 0 && h.proposals != nil {
		files := make([]proposals.File, len(w.Proposals))
		for i, f := range w.Proposals {
			files[i] = proposals.File{Name: f.Name, Data: f.Data}
		}
		// The runner already dropped the files that hold a secret.
		for _, n := range h.proposals.Submit(ctx, proposals.Submission{SessionID: sessionID, Source: snap.Source, Files: files}) {
			if n.Dropped {
				snap.Warnings = append(snap.Warnings, n.Text)
			} else {
				snap.Notes = append(snap.Notes, n.Text)
			}
		}
	}
	h.submitSchedule(ctx, sessionID, w.Schedule)
}

// JobControl returns the session's stop request and any steer messages
// queued for it, waiting up to wait (capped at MaxControlWait) for one.
func (h *Engine) JobControl(ctx context.Context, sessionID, token string, wait time.Duration) (*JobControl, error) {
	s, run, err := h.authJob(sessionID, token)
	if err != nil {
		return nil, err
	}
	if wait > MaxControlWait {
		wait = MaxControlWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		run.mu.Lock()
		if run.ended {
			run.mu.Unlock()
			return nil, ErrJobEnded
		}
		c := &JobControl{Stop: s.StopRequested(), Steer: run.steer}
		run.steer = nil
		run.mu.Unlock()
		if c.Stop || len(c.Steer) > 0 {
			return c, nil
		}
		select {
		case <-s.StopChan():
		case <-run.wake:
		case <-timer.C:
			return c, nil
		case <-ctx.Done():
			return c, nil
		}
	}
}

// queueSteer hands a steer message to a remote session's next control poll.
func (h *Engine) queueSteer(sessionID, text string) error {
	v, ok := h.remote.Load(sessionID)
	if !ok {
		return fmt.Errorf("session %s has no remote run", sessionID)
	}
	r := v.(*remoteRun)
	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()
		return fmt.Errorf("session %s has ended", sessionID)
	}
	if !r.steerable {
		// A one-shot CLI, or none running: the caller falls back (a
		// follow-up run) rather than lose the message.
		r.mu.Unlock()
		return fmt.Errorf("session %s has no live executor session", sessionID)
	}
	r.steer = append(r.steer, text)
	r.mu.Unlock()
	r.poke()
	return nil
}

// countRemoteStart counts a remote run as an active session, as
// ExecuteAgentWithContext does a local one; finalizeSession uncounts it.
func countRemoteStart(session *agent.Session) {
	source := session.Snapshot().Source
	if source == "" {
		source = "api"
	}
	metrics.ActiveSessions.WithLabelValues(source).Inc()
}
