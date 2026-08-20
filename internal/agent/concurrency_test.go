package agent

import (
	"sync"
	"testing"
	"time"
)

// runTracker builds a startFunc that records concurrency and blocks until
// release is closed. peak reports the highest number of simultaneous runs.
type runTracker struct {
	mu      sync.Mutex
	running int
	peak    int
	started chan struct{}
	release chan struct{}
}

func newRunTracker(capacity int) *runTracker {
	return &runTracker{
		started: make(chan struct{}, capacity),
		release: make(chan struct{}),
	}
}

func (r *runTracker) fn(_ *Session) {
	r.mu.Lock()
	r.running++
	if r.running > r.peak {
		r.peak = r.running
	}
	r.mu.Unlock()

	r.started <- struct{}{}
	<-r.release

	r.mu.Lock()
	r.running--
	r.mu.Unlock()
}

func (r *runTracker) peakValue() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

// TestDispatch_RespectsMaxConcurrent covers both halves of the pool contract:
// maxConcurrent sessions run at once, and the next one waits for a free worker.
func TestDispatch_RespectsMaxConcurrent(t *testing.T) {
	const max = 3
	mgr := NewManager(3600, 10, max)
	defer mgr.Stop()

	tr := newRunTracker(8)
	for range max + 1 {
		s, _ := mgr.CreateSession("concurrent", nil, "", "", 1, 60)
		if err := mgr.Enqueue(s, tr.fn); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	for range max {
		select {
		case <-tr.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d sessions started", tr.peakValue(), max)
		}
	}

	// The fourth must stay queued while all workers are occupied.
	select {
	case <-tr.started:
		t.Fatal("a fourth session started while maxConcurrent was 3")
	case <-time.After(200 * time.Millisecond):
	}

	if got := tr.peakValue(); got != max {
		t.Errorf("peak concurrency = %d, want %d", got, max)
	}

	// Freeing the workers lets the queued one through.
	close(tr.release)
	select {
	case <-tr.started:
	case <-time.After(2 * time.Second):
		t.Fatal("queued session never ran after a worker freed up")
	}
}

// TestDispatch_DefaultIsSerial pins the shipped default: without
// AGENT_MAX_CONCURRENT, dispatch behaves exactly as it did before the pool.
func TestDispatch_DefaultIsSerial(t *testing.T) {
	mgr := NewManager(3600, 10, 1)
	defer mgr.Stop()

	tr := newRunTracker(8)
	for range 3 {
		s, _ := mgr.CreateSession("serial", nil, "", "", 1, 60)
		if err := mgr.Enqueue(s, tr.fn); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	select {
	case <-tr.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first session never started")
	}
	select {
	case <-tr.started:
		t.Fatal("a second session ran concurrently with maxConcurrent=1")
	case <-time.After(200 * time.Millisecond):
	}
	if got := tr.peakValue(); got != 1 {
		t.Errorf("peak concurrency = %d, want 1", got)
	}
	close(tr.release)
}

func TestNewManager_ClampsMaxConcurrent(t *testing.T) {
	for _, in := range []int{0, -1} {
		mgr := NewManager(3600, 10, in)
		if got := mgr.MaxConcurrent(); got != 1 {
			t.Errorf("NewManager(maxConcurrent=%d).MaxConcurrent() = %d, want 1", in, got)
		}
		mgr.Stop()
	}
}

func TestRunningCount(t *testing.T) {
	mgr := NewManager(3600, 10, 2)
	defer mgr.Stop()

	if got := mgr.RunningCount(); got != 0 {
		t.Fatalf("RunningCount() on idle manager = %d, want 0", got)
	}

	tr := newRunTracker(4)
	for range 2 {
		s, _ := mgr.CreateSession("counted", nil, "", "", 1, 60)
		if err := mgr.Enqueue(s, tr.fn); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	for range 2 {
		<-tr.started
	}
	if got := mgr.RunningCount(); got != 2 {
		t.Errorf("RunningCount() = %d, want 2", got)
	}

	close(tr.release)
	waitFor(t, "running count to fall back to zero", func() bool {
		return mgr.RunningCount() == 0
	})
}

// TestStop_DrainsOnceWithManyWorkers guards the drainOnce guard: N idle workers
// all see stopCh close at the same time, and only one may drain. Draining twice
// would fail an already-failed session.
func TestStop_DrainsOnceWithManyWorkers(t *testing.T) {
	mgr := NewManager(3600, 10, 4)

	// Occupy every worker so the enqueued items below stay in the buffer.
	tr := newRunTracker(8)
	for range 4 {
		s, _ := mgr.CreateSession("occupier", nil, "", "", 1, 60)
		if err := mgr.Enqueue(s, tr.fn); err != nil {
			t.Fatalf("enqueue occupier: %v", err)
		}
	}
	for range 4 {
		<-tr.started
	}

	var queued []*Session
	for range 3 {
		s, _ := mgr.CreateSession("drained", nil, "", "", 1, 60)
		if err := mgr.Enqueue(s, func(*Session) {}); err != nil {
			t.Fatalf("enqueue drained: %v", err)
		}
		queued = append(queued, s)
	}

	close(tr.release)
	if !mgr.StopAndWait(3 * time.Second) {
		t.Fatal("dispatch workers did not return after Stop")
	}

	for _, s := range queued {
		snap, _ := mgr.GetSession(s.ID)
		if snap.Status == SessionStatusQueued {
			t.Errorf("session %s still queued after drain", s.ID)
		}
	}
}
