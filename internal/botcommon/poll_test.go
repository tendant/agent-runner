package botcommon

import (
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// withFastPolling shrinks the package's poll-loop tunables for the duration
// of a test so it doesn't wait on real 5-second ticks / hour-long deadlines,
// then restores the production defaults. fallbackTimeout must stay >= 1s:
// PollAndReport converts it with int(fallbackTimeout.Seconds()) (matching
// the original per-bot implementations this replaced), which truncates any
// sub-second value to a spurious zero-duration deadline.
func withFastPolling(t *testing.T) {
	t.Helper()
	origInterval, origFallback, origGrace := pollInterval, fallbackTimeout, deadlineGrace
	pollInterval = 10 * time.Millisecond
	fallbackTimeout = 1 * time.Second
	deadlineGrace = 0
	t.Cleanup(func() {
		pollInterval, fallbackTimeout, deadlineGrace = origInterval, origFallback, origGrace
	})
}

// fakeStarter returns a scripted sequence of session states, one per call to
// GetAgentSession, repeating the final state once the script is exhausted so
// a test doesn't need to predict exactly how many ticks will elapse.
type fakeStarter struct {
	mu      sync.Mutex
	states  []*agent.Session
	calls   int
	missing bool // if true, GetAgentSession always reports not-found
}

func (f *fakeStarter) StartAgent(message, source, _ string) (string, error) { return "", nil }

func (f *fakeStarter) GetAgentSession(sessionID string) (*agent.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing {
		return nil, false
	}
	idx := f.calls
	if idx >= len(f.states) {
		idx = len(f.states) - 1
	}
	f.calls++
	return f.states[idx], true
}

// recordingReporter implements Reporter and records every callback it
// receives, guarded by a mutex since PollAndReport runs on its own goroutine
// in these tests.
type recordingReporter struct {
	mu        sync.Mutex
	completed []agent.IterationResult
	starts    [][2]int
	final     *agent.Session
	notFound  bool
	timedOut  bool
	done      chan struct{}
}

func newRecordingReporter() *recordingReporter {
	return &recordingReporter{done: make(chan struct{})}
}

func (r *recordingReporter) OnIterationComplete(iter agent.IterationResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed = append(r.completed, iter)
}

func (r *recordingReporter) OnIterationStart(current, max int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, [2]int{current, max})
}

func (r *recordingReporter) OnFinal(session *agent.Session) {
	r.mu.Lock()
	r.final = session
	r.mu.Unlock()
	close(r.done)
}

func (r *recordingReporter) OnNotFound() {
	r.mu.Lock()
	r.notFound = true
	r.mu.Unlock()
	close(r.done)
}

func (r *recordingReporter) OnTimeout() {
	r.mu.Lock()
	r.timedOut = true
	r.mu.Unlock()
	close(r.done)
}

// waitDone blocks until the reporter records a terminal callback (OnFinal,
// OnNotFound, or OnTimeout), failing the test if that never happens.
func waitDone(t *testing.T, r *recordingReporter) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("PollAndReport did not return within 3s")
	}
}

func TestPollAndReport_IterationCompleteThenFinal(t *testing.T) {
	withFastPolling(t)

	iter1 := agent.IterationResult{Iteration: 1, Status: agent.IterationStatusSuccess}
	iter2 := agent.IterationResult{Iteration: 2, Status: agent.IterationStatusSuccess}
	starter := &fakeStarter{states: []*agent.Session{
		{Status: agent.SessionStatusRunning, Iterations: []agent.IterationResult{iter1}},
		{Status: agent.SessionStatusRunning, Iterations: []agent.IterationResult{iter1, iter2}},
		{Status: agent.SessionStatusCompleted, Iterations: []agent.IterationResult{iter1, iter2}, ElapsedSeconds: 5},
	}}
	r := newRecordingReporter()

	go PollAndReport(starter, "s1", r)
	waitDone(t, r)

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.completed) != 2 || r.completed[0].Iteration != 1 || r.completed[1].Iteration != 2 {
		t.Errorf("expected iterations 1,2 reported in order, got: %+v", r.completed)
	}
	if r.final == nil || r.final.Status != agent.SessionStatusCompleted {
		t.Errorf("expected OnFinal with completed session, got: %+v", r.final)
	}
}

func TestPollAndReport_IterationStart(t *testing.T) {
	withFastPolling(t)

	starter := &fakeStarter{states: []*agent.Session{
		{Status: agent.SessionStatusRunning, CurrentIteration: 1, MaxIterations: 3},
		{Status: agent.SessionStatusRunning, CurrentIteration: 2, MaxIterations: 3},
		{Status: agent.SessionStatusFailed, CurrentIteration: 2, MaxIterations: 3},
	}}
	r := newRecordingReporter()

	go PollAndReport(starter, "s1", r)
	waitDone(t, r)

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.starts) != 2 || r.starts[0] != [2]int{1, 3} || r.starts[1] != [2]int{2, 3} {
		t.Errorf("expected iteration-start callbacks for 1 and 2, got: %+v", r.starts)
	}
}

// TestPollAndReport_ReportsBeforeFinal is the regression test for the
// ordering bug found in stream's original pollAndReport: when a tick
// observes both a newly-started/completed iteration AND a terminal status
// simultaneously, the iteration callback must fire before OnFinal so the
// last iteration's progress is never silently dropped.
func TestPollAndReport_ReportsBeforeFinal(t *testing.T) {
	withFastPolling(t)

	finalIter := agent.IterationResult{Iteration: 1, Status: agent.IterationStatusSuccess}
	starter := &fakeStarter{states: []*agent.Session{
		// Single tick: iteration 1 both completed AND current, session already terminal.
		{
			Status:           agent.SessionStatusCompleted,
			Iterations:       []agent.IterationResult{finalIter},
			CurrentIteration: 1,
			MaxIterations:    1,
			ElapsedSeconds:   3,
		},
	}}
	r := newRecordingReporter()

	go PollAndReport(starter, "s1", r)
	waitDone(t, r)

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.completed) != 1 {
		t.Fatalf("expected 1 completed iteration reported, got %d", len(r.completed))
	}
	if len(r.starts) != 1 {
		t.Fatalf("expected 1 iteration-start reported, got %d", len(r.starts))
	}
	if r.final == nil {
		t.Fatal("expected OnFinal to be called")
	}
}

func TestPollAndReport_NotFound(t *testing.T) {
	withFastPolling(t)

	starter := &fakeStarter{missing: true}
	r := newRecordingReporter()

	go PollAndReport(starter, "missing-session", r)
	waitDone(t, r)

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.notFound {
		t.Error("expected OnNotFound to be called")
	}
	if r.final != nil {
		t.Error("expected OnFinal not to be called")
	}
}

func TestPollAndReport_Timeout(t *testing.T) {
	withFastPolling(t)

	// Session never reaches a terminal status; MaxTotalSeconds is tiny so the
	// (shrunk) deadline fires quickly.
	starter := &fakeStarter{states: []*agent.Session{
		{Status: agent.SessionStatusRunning, MaxTotalSeconds: 0},
	}}
	r := newRecordingReporter()

	go PollAndReport(starter, "s1", r)
	waitDone(t, r)

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.timedOut {
		t.Error("expected OnTimeout to be called")
	}
}
