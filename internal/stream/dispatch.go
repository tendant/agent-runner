package stream

import (
	"sync"
)

// defaultMaxThreadHandlers bounds how many threads have a message being
// handled at once. Handling can mean an analyzer LLM call or file downloads,
// so a reconnect backlog must not fan out into an unbounded burst of calls.
const defaultMaxThreadHandlers = 8

// threadDispatcher runs each thread's messages in arrival order on their own
// goroutine, so a slow analysis or download in one thread does not hold up
// another. Dispatch never blocks the caller (the SSE loop); a thread with no
// queued work holds no goroutine.
type threadDispatcher struct {
	mu      sync.Mutex
	pending map[string][]func()
	running map[string]bool
	slots   chan struct{}
	wg      *sync.WaitGroup
}

func newThreadDispatcher(maxConcurrent int, wg *sync.WaitGroup) *threadDispatcher {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxThreadHandlers
	}
	return &threadDispatcher{
		pending: make(map[string][]func()),
		running: make(map[string]bool),
		slots:   make(chan struct{}, maxConcurrent),
		wg:      wg,
	}
}

// Dispatch queues fn behind any earlier work for key.
func (d *threadDispatcher) Dispatch(key string, fn func()) {
	d.mu.Lock()
	d.pending[key] = append(d.pending[key], fn)
	if d.running[key] {
		d.mu.Unlock()
		return
	}
	d.running[key] = true
	d.mu.Unlock()

	d.wg.Add(1)
	go d.drain(key)
}

func (d *threadDispatcher) drain(key string) {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		queue := d.pending[key]
		if len(queue) == 0 {
			delete(d.pending, key)
			delete(d.running, key)
			d.mu.Unlock()
			return
		}
		fn := queue[0]
		d.pending[key] = queue[1:]
		d.mu.Unlock()

		d.slots <- struct{}{}
		fn()
		<-d.slots
	}
}

// cursorTracker decides how far a channel's persisted event cursor may move
// while messages are handled concurrently: never past a message whose
// handling has not finished, so a restart re-delivers in-flight messages
// instead of dropping them. The saved value only ever increases.
type cursorTracker struct {
	mu       sync.Mutex
	inflight map[int64]struct{}
	highest  int64 // highest seq seen
	saved    int64
	save     func(int64)
}

func newCursorTracker(start int64, save func(int64)) *cursorTracker {
	return &cursorTracker{inflight: make(map[int64]struct{}), highest: start, saved: start, save: save}
}

// begin marks seq as dispatched but not yet handled.
func (t *cursorTracker) begin(seq int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight[seq] = struct{}{}
	if seq > t.highest {
		t.highest = seq
	}
}

// advance records an event that needs no handling (or was skipped).
func (t *cursorTracker) advance(seq int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if seq > t.highest {
		t.highest = seq
	}
	t.persistLocked()
}

// done marks a dispatched seq as handled.
func (t *cursorTracker) done(seq int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, seq)
	t.persistLocked()
}

// safe is the highest seq with nothing unhandled at or below it.
func (t *cursorTracker) safe() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.safeLocked()
}

func (t *cursorTracker) safeLocked() int64 {
	safe := t.highest
	for seq := range t.inflight {
		if seq-1 < safe {
			safe = seq - 1
		}
	}
	return safe
}

func (t *cursorTracker) persistLocked() {
	if s := t.safeLocked(); s > t.saved {
		t.saved = s
		t.save(s)
	}
}
