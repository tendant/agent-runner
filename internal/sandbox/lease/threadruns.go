package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrQueueFull is returned when too many runs already wait on a Thread.
var ErrQueueFull = errors.New("thread run queue full")

// ThreadRuns enforces one active run per Thread with a bounded FIFO queue, backed
// by a durable lease that is renewed while the run is active.
type ThreadRuns struct {
	Store    *Store
	Owner    Owner
	TTL      time.Duration // lease ttl; renewed every TTL/3
	MaxQueue int           // waiters allowed per Thread (excluding the active run)
	OnLost   func(thread string)

	mu      sync.Mutex
	threads map[string]*threadState
}

type threadState struct {
	active  bool
	waiters []chan struct{}
}

// Enter waits for the Thread, takes the durable lease, and returns a context
// that is cancelled if the lease is lost, plus a release function.
func (t *ThreadRuns) Enter(ctx context.Context, thread, runID string) (context.Context, func(), error) {
	t.mu.Lock()
	if t.threads == nil {
		t.threads = map[string]*threadState{}
	}
	st := t.threads[thread]
	if st == nil {
		st = &threadState{}
		t.threads[thread] = st
	}
	if st.active {
		if len(st.waiters) >= t.MaxQueue {
			t.mu.Unlock()
			return nil, nil, ErrQueueFull
		}
		ch := make(chan struct{})
		st.waiters = append(st.waiters, ch)
		t.mu.Unlock()
		select {
		case <-ch: // ownership handed to us
		case <-ctx.Done():
			t.mu.Lock()
			for i, w := range st.waiters {
				if w == ch {
					st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
					t.mu.Unlock()
					return nil, nil, ctx.Err()
				}
			}
			t.mu.Unlock()
			// Already handed the slot: give it back.
			t.next(thread)
			return nil, nil, ctx.Err()
		}
	} else {
		st.active = true
		t.mu.Unlock()
	}

	name := "thread:" + thread
	if _, err := t.Store.Acquire(Record{Name: name, Kind: "thread", Owner: t.Owner, ThreadID: thread, RunID: runID}, t.TTL); err != nil {
		t.next(thread)
		return nil, nil, fmt.Errorf("thread %s: %w", thread, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(t.TTL / 3)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if err := t.Store.Renew(name, t.Owner, t.TTL, nil); err != nil {
					if t.OnLost != nil {
						t.OnLost(thread)
					}
					cancel() // enforcement lost: stop the run
					return
				}
			}
		}
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			cancel()
			_ = t.Store.Release(name, t.Owner)
			t.next(thread)
		})
	}
	return runCtx, release, nil
}

// next hands the Thread to the first waiter, or marks it idle.
func (t *ThreadRuns) next(thread string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.threads[thread]
	if len(st.waiters) > 0 {
		ch := st.waiters[0]
		st.waiters = st.waiters[1:]
		close(ch)
		return
	}
	st.active = false
	delete(t.threads, thread)
}
