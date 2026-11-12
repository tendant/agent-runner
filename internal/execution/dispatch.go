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
}

// JobControl answers a runner's control poll (§7.3).
type JobControl struct {
	Stop  bool     `json:"stop"`
	Steer []string `json:"steer,omitempty"`
}

// remoteRun is the dispatcher's record of a session running elsewhere.
type remoteRun struct {
	mu     sync.Mutex
	seq    uint64   // last report applied
	ended  bool     // result taken, or the run failed without one
	result bool     // the result was taken
	steer  []string // steer messages for the next control poll
	wake   chan struct{}
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
	h.remote.Store(sessionID, &remoteRun{wake: make(chan struct{}, 1)})
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
	return s, v.(*remoteRun), nil
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
func (h *Engine) JobReport(sessionID, token string, rep *JobReport) error {
	s, run, err := h.authJob(sessionID, token)
	if err != nil {
		return err
	}
	if rep.Session == nil {
		return fmt.Errorf("report without a session")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.ended {
		return ErrJobEnded
	}
	if rep.Seq <= run.seq {
		return nil
	}
	if err := s.ApplyWire(rep.Session, DecodeWire); err != nil {
		return err
	}
	run.seq = rep.Seq
	return nil
}

// JobResult takes a runner's result once: the session gets its final state
// and is finalized here, as a local run is, and the run ends. A repeat of a
// taken result reports duplicate (the runner's retry after a lost reply).
func (h *Engine) JobResult(sessionID, token string, w *RunResultWire) (duplicate bool, err error) {
	s, run, err := h.authJob(sessionID, token)
	if err != nil {
		return false, err
	}
	if w.SessionID != sessionID {
		return false, fmt.Errorf("result for %s sent to %s", w.SessionID, sessionID)
	}
	res, err := w.RunResult()
	if err != nil {
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
	run.result, run.ended = true, true
	run.mu.Unlock()
	run.poke()

	if err := s.ApplyWire(w.Snapshot, DecodeWire); err != nil {
		// Checked by RunResult above; a failure here is a bug, and the
		// session must still end.
		slog.Error("job result: applying the final state", "session_id", sessionID, "error", err)
	}
	h.agentManager.Ended(sessionID)
	h.finalizeSession(res)
	h.runEnded(sessionID)
	return false, nil
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
