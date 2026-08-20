// Package locks provides named advisory leases that agent sessions take out on
// each other.
//
// Sessions get an isolated workspace, so what they can collide over is external:
// a shared config repo, a sequential ID allocated by listing a directory, a
// remote branch. Which of those matter is a property of the task, not of the
// runner — only the agent knows whether two pieces of work touch the same thing.
// So the runner supplies the primitive and lets the agent name what it is
// protecting (see the HTTP surface in internal/api and the workflow in
// prompt.md).
//
// Leases are advisory: nothing stops a session that never asks. They are also
// in-memory and process-local — a restart clears them, which is the right
// failure mode for a lock whose holders don't survive a restart either.
package locks

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTTL applies when a caller does not ask for one.
	DefaultTTL = 10 * time.Minute
	// MaxTTL caps how long a single lease can wedge a name. A crashed or
	// runaway session must not hold a name forever; sessions that legitimately
	// need longer should re-acquire, which refreshes the deadline.
	MaxTTL = 2 * time.Hour
	// MaxWait caps how long Acquire will block.
	MaxWait = 30 * time.Minute
	// MaxNameLen bounds the key so a stray prompt can't allocate unboundedly.
	MaxNameLen = 200
)

// Lease is a held name.
type Lease struct {
	Name       string    `json:"name"`
	Holder     string    `json:"holder"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// ConflictError reports that a name is held by someone else.
type ConflictError struct {
	Held Lease
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("lock %q is held by %s until %s",
		e.Held.Name, e.Held.Holder, e.Held.ExpiresAt.Format(time.RFC3339))
}

// Manager tracks named leases.
type Manager struct {
	mu     sync.Mutex
	leases map[string]*Lease
	// changed is closed and replaced whenever a name is freed, waking anyone
	// blocked in Acquire. A fresh channel per generation means waiters never
	// miss a release that happens between their check and their wait.
	changed chan struct{}
}

// NewManager creates an empty lease manager.
func NewManager() *Manager {
	return &Manager{
		leases:  make(map[string]*Lease),
		changed: make(chan struct{}),
	}
}

// Acquire takes the lease on name for holder.
//
// ttl bounds how long the lease survives without being refreshed; wait bounds
// how long to block when someone else holds it. With wait <= 0 the call is a
// try-lock and returns *ConflictError immediately.
//
// Re-acquiring a name the caller already holds refreshes the deadline rather
// than deadlocking, so an agent that retries a step is not punished for it.
func (m *Manager) Acquire(ctx context.Context, name, holder string, ttl, wait time.Duration) (Lease, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Lease{}, fmt.Errorf("lock name is required")
	}
	if len(name) > MaxNameLen {
		return Lease{}, fmt.Errorf("lock name exceeds %d characters", MaxNameLen)
	}
	if holder == "" {
		return Lease{}, fmt.Errorf("lock holder is required")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	ttl = min(ttl, MaxTTL)
	wait = min(wait, MaxWait)

	deadline := time.Now().Add(wait)

	for {
		m.mu.Lock()
		m.reapLocked(time.Now())

		held, exists := m.leases[name]
		if !exists {
			now := time.Now()
			lease := &Lease{Name: name, Holder: holder, AcquiredAt: now, ExpiresAt: now.Add(ttl)}
			m.leases[name] = lease
			out := *lease
			m.mu.Unlock()
			return out, nil
		}

		if held.Holder == holder {
			held.ExpiresAt = time.Now().Add(ttl)
			out := *held
			m.mu.Unlock()
			return out, nil
		}

		conflict := *held
		freed := m.changed
		m.mu.Unlock()

		now := time.Now()
		if wait <= 0 || !now.Before(deadline) {
			return Lease{}, &ConflictError{Held: conflict}
		}

		// Wake on release, on the holder's TTL running out, or on our own
		// deadline — whichever comes first.
		wake := deadline
		if conflict.ExpiresAt.Before(wake) {
			wake = conflict.ExpiresAt
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-freed:
			timer.Stop()
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Lease{}, ctx.Err()
		}
	}
}

// Release frees name if holder owns it. Releasing a name held by someone else
// is refused rather than honoured: a stale release from a session that already
// timed out would otherwise yank the lease out from under its successor.
func (m *Manager) Release(name, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	held, exists := m.leases[strings.TrimSpace(name)]
	if !exists {
		return fmt.Errorf("lock %q is not held", name)
	}
	if held.Holder != holder {
		return fmt.Errorf("lock %q is held by %s, not %s", name, held.Holder, holder)
	}
	delete(m.leases, held.Name)
	m.notifyLocked()
	return nil
}

// ReleaseAll frees every lease owned by holder and returns how many there were.
// Sessions call this as they finish, so a failed or stopped run never leaves a
// name wedged until its TTL runs out.
func (m *Manager) ReleaseAll(holder string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for name, lease := range m.leases {
		if lease.Holder == holder {
			delete(m.leases, name)
			n++
		}
	}
	if n > 0 {
		m.notifyLocked()
	}
	return n
}

// List returns the currently held leases, expired ones already dropped.
func (m *Manager) List() []Lease {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.reapLocked(time.Now())
	out := make([]Lease, 0, len(m.leases))
	for _, lease := range m.leases {
		out = append(out, *lease)
	}
	return out
}

// reapLocked drops expired leases. Callers must hold mu.
func (m *Manager) reapLocked(now time.Time) {
	freed := false
	for name, lease := range m.leases {
		if now.After(lease.ExpiresAt) {
			delete(m.leases, name)
			freed = true
		}
	}
	if freed {
		m.notifyLocked()
	}
}

// notifyLocked wakes every waiter. Callers must hold mu.
func (m *Manager) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}
