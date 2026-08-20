package locks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAcquire_FreeName(t *testing.T) {
	m := NewManager()
	lease, err := m.Acquire(context.Background(), "sites-config", "session-a", time.Minute, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.Holder != "session-a" || lease.Name != "sites-config" {
		t.Errorf("unexpected lease %+v", lease)
	}
}

func TestAcquire_ConflictIsTryLockByDefault(t *testing.T) {
	m := NewManager()
	if _, err := m.Acquire(context.Background(), "sites-config", "session-a", time.Minute, 0); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	start := time.Now()
	_, err := m.Acquire(context.Background(), "sites-config", "session-b", time.Minute, 0)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %v", err)
	}
	if conflict.Held.Holder != "session-a" {
		t.Errorf("conflict names holder %q, want session-a", conflict.Held.Holder)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("try-lock blocked for %s, should return immediately", elapsed)
	}
}

func TestAcquire_SameHolderRefreshes(t *testing.T) {
	m := NewManager()
	first, err := m.Acquire(context.Background(), "site:x", "session-a", 50*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := m.Acquire(context.Background(), "site:x", "session-a", time.Minute, 0)
	if err != nil {
		t.Fatalf("re-acquire by same holder should refresh, got %v", err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) {
		t.Error("re-acquire did not extend the deadline")
	}
}

func TestAcquire_WaitsForRelease(t *testing.T) {
	m := NewManager()
	if _, err := m.Acquire(context.Background(), "sites-config", "session-a", time.Minute, 0); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var acquired Lease
	var acqErr error
	go func() {
		defer wg.Done()
		acquired, acqErr = m.Acquire(context.Background(), "sites-config", "session-b", time.Minute, 5*time.Second)
	}()

	time.Sleep(50 * time.Millisecond)
	if err := m.Release("sites-config", "session-a"); err != nil {
		t.Fatalf("release: %v", err)
	}

	wg.Wait()
	if acqErr != nil {
		t.Fatalf("waiting acquire failed: %v", acqErr)
	}
	if acquired.Holder != "session-b" {
		t.Errorf("lease went to %q, want session-b", acquired.Holder)
	}
}

func TestAcquire_WaitTimesOut(t *testing.T) {
	m := NewManager()
	if _, err := m.Acquire(context.Background(), "n", "session-a", time.Minute, 0); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := m.Acquire(context.Background(), "n", "session-b", time.Minute, 100*time.Millisecond)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError after wait, got %v", err)
	}
}

func TestAcquire_ExpiredLeaseIsReclaimed(t *testing.T) {
	m := NewManager()
	if _, err := m.Acquire(context.Background(), "n", "dead-session", 20*time.Millisecond, 0); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// A holder that never releases must not wedge the name past its TTL.
	lease, err := m.Acquire(context.Background(), "n", "session-b", time.Minute, 2*time.Second)
	if err != nil {
		t.Fatalf("expected to reclaim expired lease, got %v", err)
	}
	if lease.Holder != "session-b" {
		t.Errorf("lease holder = %q, want session-b", lease.Holder)
	}
}

func TestAcquire_ContextCancel(t *testing.T) {
	m := NewManager()
	if _, err := m.Acquire(context.Background(), "n", "session-a", time.Minute, 0); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := m.Acquire(ctx, "n", "session-b", time.Minute, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRelease_WrongHolderRefused(t *testing.T) {
	m := NewManager()
	if _, err := m.Acquire(context.Background(), "n", "session-a", time.Minute, 0); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Release("n", "session-b"); err == nil {
		t.Error("expected release by a non-holder to be refused")
	}
	if got := len(m.List()); got != 1 {
		t.Errorf("lease count = %d, want 1 (still held)", got)
	}
}

func TestReleaseAll(t *testing.T) {
	m := NewManager()
	for _, name := range []string{"a", "b", "c"} {
		if _, err := m.Acquire(context.Background(), name, "session-a", time.Minute, 0); err != nil {
			t.Fatalf("acquire %s: %v", name, err)
		}
	}
	if _, err := m.Acquire(context.Background(), "d", "session-b", time.Minute, 0); err != nil {
		t.Fatalf("acquire d: %v", err)
	}

	if n := m.ReleaseAll("session-a"); n != 3 {
		t.Errorf("ReleaseAll released %d, want 3", n)
	}
	leases := m.List()
	if len(leases) != 1 || leases[0].Holder != "session-b" {
		t.Errorf("remaining leases = %+v, want only session-b's", leases)
	}
}

func TestAcquire_Validation(t *testing.T) {
	m := NewManager()
	if _, err := m.Acquire(context.Background(), "  ", "session-a", time.Minute, 0); err == nil {
		t.Error("expected error for blank name")
	}
	if _, err := m.Acquire(context.Background(), "n", "", time.Minute, 0); err == nil {
		t.Error("expected error for blank holder")
	}
	long := make([]byte, MaxNameLen+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := m.Acquire(context.Background(), string(long), "session-a", time.Minute, 0); err == nil {
		t.Error("expected error for over-long name")
	}
}

func TestAcquire_TTLCapped(t *testing.T) {
	m := NewManager()
	lease, err := m.Acquire(context.Background(), "n", "session-a", 100*time.Hour, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.ExpiresAt.After(time.Now().Add(MaxTTL + time.Minute)) {
		t.Errorf("TTL was not capped at %s", MaxTTL)
	}
}

// TestAcquire_OnlyOneWinnerUnderContention is the property the whole package
// exists for: N sessions racing for the same name, exactly one proceeds.
func TestAcquire_OnlyOneWinnerUnderContention(t *testing.T) {
	m := NewManager()
	const racers = 20

	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0

	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := m.Acquire(context.Background(), "sites-config", holderName(i), time.Minute, 0)
			if err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d sessions acquired the same name, want exactly 1", winners)
	}
}

func holderName(i int) string {
	return "session-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}
