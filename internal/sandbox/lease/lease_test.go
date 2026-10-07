package lease

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func store(t *testing.T) *Store {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// deadOwner returns an Owner whose process has exited.
func deadOwner(t *testing.T) Owner {
	c := exec.Command("true")
	c.Start()
	o := Owner{InstanceID: "dead", PID: c.Process.Pid, Start: ProcStart(c.Process.Pid)}
	c.Wait()
	return o
}

func TestAcquireRules(t *testing.T) {
	s := store(t)
	me := SelfOwner("A")
	if _, err := s.Acquire(Record{Name: "thread:1", Kind: "thread", Owner: me}, time.Minute); err != nil {
		t.Fatal(err)
	}
	// A different instance ID in the same live process is still "someone else" while unexpired.
	other := SelfOwner("B")
	if _, err := s.Acquire(Record{Name: "thread:1", Owner: other}, time.Minute); !errors.Is(err, ErrHeld) {
		t.Errorf("unexpired lease must be held: %v", err)
	}
	// Expired but owner ALIVE: still not takeable (never reap a live runner).
	s.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := s.Acquire(Record{Name: "thread:1", Owner: other}, time.Minute); !errors.Is(err, ErrHeld) {
		t.Errorf("expired lease with live owner must not be taken: %v", err)
	}
	// Expired and owner dead: takeover allowed.
	s.Now = nil
	d := deadOwner(t)
	s.Acquire(Record{Name: "thread:2", Owner: d}, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, err := s.Acquire(Record{Name: "thread:2", Owner: me}, time.Minute); err != nil {
		t.Errorf("takeover from dead owner: %v", err)
	}
	// Renew by non-owner is lost.
	if err := s.Renew("thread:2", d, time.Minute, nil); !errors.Is(err, ErrLost) {
		t.Errorf("renew by old owner: %v", err)
	}
	if err := s.Release("thread:2", me); err != nil {
		t.Error(err)
	}
}

func TestThreadRunsSerializesAndBoundsQueue(t *testing.T) {
	tr := &ThreadRuns{Store: store(t), Owner: SelfOwner("A"), TTL: time.Second, MaxQueue: 1}
	ctx := context.Background()
	_, rel1, err := tr.Enter(ctx, "th", "r1")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	done := make(chan struct{})
	go func() {
		_, rel2, err := tr.Enter(ctx, "th", "r2")
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		order = append(order, "r2")
		mu.Unlock()
		rel2()
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	// Queue (size 1) is occupied by r2: r3 is rejected.
	if _, _, err := tr.Enter(ctx, "th", "r3"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("want queue full, got %v", err)
	}
	// Other threads are independent.
	if _, rel, err := tr.Enter(ctx, "other", "x"); err != nil {
		t.Error(err)
	} else {
		rel()
	}
	mu.Lock()
	if len(order) != 0 {
		t.Error("r2 must wait for r1")
	}
	mu.Unlock()
	rel1()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("r2 never ran")
	}
}

func TestQueuedWaiterCancel(t *testing.T) {
	tr := &ThreadRuns{Store: store(t), Owner: SelfOwner("A"), TTL: time.Second, MaxQueue: 2}
	_, rel, _ := tr.Enter(context.Background(), "th", "r1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := tr.Enter(ctx, "th", "r2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v", err)
	}
	rel()
	// Slot is free again afterwards.
	_, rel3, err := tr.Enter(context.Background(), "th", "r3")
	if err != nil {
		t.Fatal(err)
	}
	rel3()
}

func TestLostLeaseCancelsRun(t *testing.T) {
	st := store(t)
	lost := make(chan string, 1)
	tr := &ThreadRuns{Store: st, Owner: SelfOwner("A"), TTL: 150 * time.Millisecond, MaxQueue: 1, OnLost: func(th string) { lost <- th }}
	runCtx, rel, err := tr.Enter(context.Background(), "th", "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	st.Remove("thread:th") // someone removed the lease
	select {
	case <-runCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("run must be cancelled when the lease is lost")
	}
	if th := <-lost; th != "th" {
		t.Error(th)
	}
}
