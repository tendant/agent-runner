package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/proposals"
)

// The runner's half of the job protocol (JOBS_DESIGN.md §7): `agent-runner
// run-session` fetches its session from the dispatcher, runs it here,
// reports as it goes, follows the dispatcher's stop and steer, and sends the
// result, which the dispatcher finalizes.

// reportInterval coalesces reports: at most one per interval (§7.2).
const reportInterval = 2 * time.Second

// controlWait is how long each control poll waits on the dispatcher.
const controlWait = 25 * time.Second

// deferredOutputs is what a runner leaves to the dispatcher (§8.4).
type deferredOutputs struct {
	Schedule  []ScheduleEntry
	Proposals []proposals.File
}

func (h *Engine) deferred(sessionID string) *deferredOutputs {
	v, _ := h.deferredOutputs.LoadOrStore(sessionID, &deferredOutputs{})
	return v.(*deferredOutputs)
}

// deferredProposals stands in for the proposals service in a runner: it
// says whether sessions may propose (so the prompt is the same), and the
// files go to the dispatcher (collectProposals).
type deferredProposals struct{ enabled bool }

func (p deferredProposals) Enabled() bool { return p.enabled }
func (deferredProposals) Submit(context.Context, proposals.Submission) []proposals.Note {
	return nil
}

// EnableRunnerMode makes this engine a session runner: outputs, proposals
// and schedules go to the dispatcher in the result instead of being handled
// here. proposalsEnabled is the dispatcher's AGENT_PROPOSALS.
func (h *Engine) EnableRunnerMode(proposalsEnabled bool) {
	h.runnerMode = true
	h.proposals = deferredProposals{enabled: proposalsEnabled}
}

// JobClient is a runner's connection to its dispatcher.
type JobClient struct {
	URL       string // the dispatcher's base URL
	SessionID string
	Token     string
	// RetryFor is how long a request that cannot reach the dispatcher is
	// retried (AGENT_JOB_REPORT_RETRY); the result's retries matter most.
	RetryFor time.Duration
	HTTP     *http.Client
}

func (c *JobClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// errRetry marks a failure worth retrying: the dispatcher unreachable or
// answering 5xx.
type errRetry struct{ err error }

func (e errRetry) Error() string { return e.err.Error() }
func (e errRetry) Unwrap() error { return e.err }

// do makes one request. 401 is ErrJobAuth and 410 ErrJobEnded, as the
// dispatcher means them; a network error or 5xx is retryable.
func (c *JobClient) do(ctx context.Context, method, op string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := strings.TrimRight(c.URL, "/") + JobPathPrefix + url.PathEscape(c.SessionID) + "/" + op
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := c.client().Do(req)
	if err != nil {
		return errRetry{err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrJobAuth
	case resp.StatusCode == http.StatusGone:
		return ErrJobEnded
	case resp.StatusCode >= 500:
		return errRetry{fmt.Errorf("%s %s: %s: %s", method, op, resp.Status, bytes.TrimSpace(data))}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s %s: %s: %s", method, op, resp.Status, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// retry runs a request until it succeeds, fails for good, or RetryFor has
// passed, backing off from 1s to 30s.
func (c *JobClient) retry(ctx context.Context, what string, try func() error) error {
	deadline := time.Now().Add(c.RetryFor)
	wait := time.Second
	for {
		err := try()
		var r errRetry
		if err == nil || !errors.As(err, &r) || time.Now().After(deadline) {
			return err
		}
		slog.Warn("runner: dispatcher unreachable, retrying", "op", what, "session_id", c.SessionID, "in", wait, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		if wait *= 2; wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
}

// Spec fetches the session to run (§7.1).
func (c *JobClient) Spec(ctx context.Context) (*agent.SessionWire, error) {
	var w agent.SessionWire
	err := c.retry(ctx, "spec", func() error { return c.do(ctx, http.MethodGet, "spec", nil, &w) })
	return &w, err
}

// Report sends one progress report (§7.2), once.
func (c *JobClient) Report(ctx context.Context, rep *JobReport) error {
	return c.do(ctx, http.MethodPost, "report", rep, nil)
}

// Result sends the result (§7.2), retried.
func (c *JobClient) Result(ctx context.Context, res *RunResultWire) error {
	return c.retry(ctx, "result", func() error { return c.do(ctx, http.MethodPost, "result", res, nil) })
}

// Control polls for a stop or steer messages (§7.3).
func (c *JobClient) Control(ctx context.Context, wait time.Duration) (*JobControl, error) {
	var ctl JobControl
	err := c.do(ctx, http.MethodGet, "control?wait="+wait.String(), nil, &ctl)
	return &ctl, err
}

// RunRemote runs the session c names, for its dispatcher, and returns once
// the result is delivered. It fails when the session cannot be fetched, or
// its result cannot be delivered within c.RetryFor: the dispatcher's
// heartbeat then finds the run gone (§9).
func (h *Engine) RunRemote(ctx context.Context, c *JobClient) error {
	w, err := c.Spec(ctx)
	if err != nil {
		return fmt.Errorf("fetching session %s: %w", c.SessionID, err)
	}
	if w.ID != c.SessionID {
		return fmt.Errorf("the dispatcher sent session %s for %s", w.ID, c.SessionID)
	}
	session, err := agent.NewSessionFromWire(w, DecodeWire)
	if err != nil {
		return err
	}
	if session.Source == "eval" && session.Eval == nil {
		return fmt.Errorf("session %s is an eval without its spec", session.ID)
	}
	if err := h.agentManager.RestoreSession(session); err != nil {
		return err
	}

	// The run stops early if the dispatcher has ended it (410).
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	loops, stopLoops := context.WithCancel(runCtx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.reportLoop(loops, c, session, cancelRun) }()
	go func() { defer wg.Done(); h.controlLoop(loops, c, session, cancelRun) }()

	rep := &remoteReporter{c: c, maxOutput: h.config.Agent.JobMaxOutputBytes, stopLoops: func() { stopLoops(); wg.Wait() }}
	h.runSession(runCtx, session, rep)
	rep.stopLoops()
	return rep.err
}

// reportLoop sends the session's state whenever it changes, at most once
// per reportInterval. A failed report is not retried: the next one carries
// the newer state anyway.
func (h *Engine) reportLoop(ctx context.Context, c *JobClient, s *agent.Session, endRun context.CancelFunc) {
	var seq uint64
	tick := time.NewTicker(reportInterval)
	defer tick.Stop()
	changed := true // the dispatcher has not seen the runner's own state yet
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Subscribe():
			changed = true
			continue
		case <-tick.C:
		}
		if !changed {
			continue
		}
		w, err := s.Wire()
		if err != nil {
			slog.Warn("runner: encoding a report", "session_id", s.ID, "error", err)
			continue
		}
		seq++
		switch err := c.Report(ctx, &JobReport{Seq: seq, Session: w}); {
		case err == nil:
			changed = false
		case errors.Is(err, ErrJobEnded), errors.Is(err, ErrJobAuth):
			slog.Warn("runner: the dispatcher ended this run", "session_id", s.ID, "error", err)
			endRun()
			return
		case ctx.Err() == nil:
			slog.Warn("runner: report failed", "session_id", s.ID, "error", err)
		}
	}
}

// controlLoop follows the dispatcher's stop and steer messages.
func (h *Engine) controlLoop(ctx context.Context, c *JobClient, s *agent.Session, endRun context.CancelFunc) {
	for ctx.Err() == nil {
		ctl, err := c.Control(ctx, controlWait)
		switch {
		case err == nil:
		case errors.Is(err, ErrJobEnded), errors.Is(err, ErrJobAuth):
			slog.Warn("runner: the dispatcher ended this run", "session_id", s.ID, "error", err)
			endRun()
			return
		default:
			if ctx.Err() == nil {
				slog.Warn("runner: control poll failed", "session_id", s.ID, "error", err)
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
			}
			continue
		}
		for _, text := range ctl.Steer {
			if err := h.Steer(s.ID, text); err != nil {
				slog.Warn("runner: steer not delivered", "session_id", s.ID, "error", err)
			}
		}
		if ctl.Stop && !s.StopRequested() {
			slog.Info("runner: stop requested by the dispatcher", "session_id", s.ID)
			s.RequestStop()
		}
	}
}

// remoteReporter sends the result to the dispatcher, which finalizes it.
type remoteReporter struct {
	c         *JobClient
	maxOutput int
	stopLoops func()
	err       error
}

func (r *remoteReporter) Finish(ctx context.Context, res *RunResult) {
	// No more reports: the result carries the final state, and a report
	// after it would only be refused.
	r.stopLoops()
	w, err := res.Wire()
	if err != nil {
		r.err = fmt.Errorf("encoding the result: %w", err)
		slog.Error("runner: result not sent", "session_id", res.SessionID, "error", err)
		return
	}
	capOutputs(w.Snapshot, r.maxOutput)
	if err := r.c.Result(ctx, w); err != nil {
		r.err = fmt.Errorf("sending the result: %w", err)
		slog.Error("runner: result not delivered", "session_id", res.SessionID, "error", err)
	}
}

// capOutputs keeps the _send/ files within max bytes: files past it are
// sent without their content, with a warning.
func capOutputs(snap *agent.SessionWire, max int) {
	if max <= 0 {
		return
	}
	total := 0
	for i := range snap.OutputFiles {
		f := &snap.OutputFiles[i]
		if total+len(f.Data) > max {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("_send/%s: not sent back, the run's files are over %d bytes (AGENT_JOB_MAX_OUTPUT_BYTES)", f.Name, max))
			f.Data = nil
			continue
		}
		total += len(f.Data)
	}
}

// RunnerMode reports whether this engine is a session runner.
func (h *Engine) RunnerMode() bool { return h.runnerMode }
