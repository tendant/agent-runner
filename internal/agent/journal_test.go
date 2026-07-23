package agent

import (
	"sync"
	"testing"
	"time"
)

// recordingJournal captures hook invocations, mutex-guarded (dispatchLoop
// calls RecordRunning from its own goroutine).
type recordingJournal struct {
	mu      sync.Mutex
	queued  []string
	running []string
	removed []string
}

func (r *recordingJournal) RecordQueued(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queued = append(r.queued, s.ID)
}
func (r *recordingJournal) RecordRunning(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = append(r.running, s.ID)
}
func (r *recordingJournal) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, id)
}
func (r *recordingJournal) counts() (q, run, rem int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queued), len(r.running), len(r.removed)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestJournalHooks_QueuedRunningRemoved(t *testing.T) {
	m := NewManager(60, 5)
	defer m.Stop()
	j := &recordingJournal{}
	m.SetJournal(j)

	session, err := m.CreateSession("task", nil, "author", "", 1, 60)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	if err := m.Enqueue(session, func(s *Session) { close(done) }); err != nil {
		t.Fatal(err)
	}

	q, _, _ := j.counts()
	if q != 1 {
		t.Errorf("expected RecordQueued on Enqueue, got %d", q)
	}

	<-done
	waitFor(t, "RecordRunning", func() bool { _, r, _ := j.counts(); return r == 1 })

	m.CompleteSession(session.ID)
	waitFor(t, "Remove on complete", func() bool { _, _, rem := j.counts(); return rem == 1 })
}

func TestJournalHooks_RemovedOnFailAndStop(t *testing.T) {
	m := NewManager(60, 5)
	defer m.Stop()
	j := &recordingJournal{}
	m.SetJournal(j)

	s1, _ := m.CreateSession("t1", nil, "a", "", 1, 60)
	s2, _ := m.CreateSession("t2", nil, "a", "", 1, 60)

	m.FailSession(s1.ID, "boom")
	m.MarkSessionStopped(s2.ID, "user stop")

	_, _, rem := j.counts()
	if rem != 2 {
		t.Errorf("expected Remove for fail and stop, got %d", rem)
	}
}

func TestJournalHooks_DrainRemovesQueued(t *testing.T) {
	// Construct a manager without its background loops so drainQueue can be
	// exercised deterministically (the live dispatch loop's stop-vs-dequeue
	// select is pseudo-random when both are ready).
	j := &recordingJournal{}
	m := &Manager{
		sessions: make(map[string]*Session),
		queue:    make(chan *queueItem, 5),
		journal:  j,
	}

	queued := &Session{ID: "agent-queued", Status: SessionStatusQueued, notify: make(chan struct{}, 1)}
	m.queue <- &queueItem{session: queued, startFunc: func(*Session) {}}

	m.drainQueue()

	if queued.Snapshot().Status != SessionStatusFailed {
		t.Errorf("drained session should be failed, got %s", queued.Snapshot().Status)
	}
	_, _, rem := j.counts()
	if rem != 1 {
		t.Errorf("expected Remove for the drained session, got %d", rem)
	}
}

func TestJournalHooks_NilJournalSafe(t *testing.T) {
	m := NewManager(60, 5)
	defer m.Stop()

	session, _ := m.CreateSession("task", nil, "a", "", 1, 60)
	done := make(chan struct{})
	if err := m.Enqueue(session, func(s *Session) { close(done) }); err != nil {
		t.Fatal(err)
	}
	<-done
	m.CompleteSession(session.ID) // must not panic without a journal
}
