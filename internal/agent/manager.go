package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/agent-runner/agent-runner/internal/metrics"
)

// Journal persists queued/running sessions so a restarted server can recover
// them. Implemented by sessionjournal; nil-safe (no journal = no persistence).
// Hooks receive snapshots — safe to read without the session lock.
type Journal interface {
	RecordQueued(snapshot *Session)
	RecordRunning(snapshot *Session)
	Remove(sessionID string)
}

// queueItem pairs a session with the function to start it.
type queueItem struct {
	session   *Session
	startFunc func(*Session)
}

// Manager manages agent sessions
type Manager struct {
	mu                      sync.RWMutex
	sessions                map[string]*Session
	stopCh                  chan struct{}
	sessionRetentionSeconds int
	ctx                     context.Context
	cancel                  context.CancelFunc
	queue                   chan *queueItem
	queueSize               int
	maxConcurrent           int
	journal                 Journal // optional; see SetJournal

	workers   sync.WaitGroup // dispatch goroutines, so Stop can wait for them
	drainOnce sync.Once      // only one worker drains the queue on shutdown
	running   atomic.Int64   // sessions currently inside startFunc
}

// NewManager creates a new agent session manager.
// sessionRetentionSeconds controls how long completed sessions are kept before cleanup.
// maxQueueSize controls the bounded queue for agent sessions.
// maxConcurrent controls how many sessions may run at once; 1 (the default,
// AGENT_MAX_CONCURRENT) dispatches them strictly one at a time.
//
// Anything a session touches outside its own workspace has to tolerate
// maxConcurrent > 1 — see internal/template for the memory-dir lock and
// internal/executor for repo-cache write-back.
func NewManager(sessionRetentionSeconds, maxQueueSize, maxConcurrent int) *Manager {
	if maxQueueSize <= 0 {
		maxQueueSize = 10
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		sessions:                make(map[string]*Session),
		stopCh:                  make(chan struct{}),
		sessionRetentionSeconds: sessionRetentionSeconds,
		ctx:                     ctx,
		cancel:                  cancel,
		queue:                   make(chan *queueItem, maxQueueSize),
		queueSize:               maxQueueSize,
		maxConcurrent:           maxConcurrent,
	}
	metrics.MaxConcurrentSessions.Set(float64(maxConcurrent))
	go m.cleanupLoop()
	m.workers.Add(maxConcurrent)
	for range maxConcurrent {
		go m.dispatchLoop()
	}
	return m
}

// SetJournal wires the session journal. Call before any session is enqueued.
func (m *Manager) SetJournal(j Journal) {
	m.journal = j
}

// Context returns the manager's context, which is cancelled on Stop().
func (m *Manager) Context() context.Context {
	return m.ctx
}

// Stop cancels the manager context, stops the cleanup and dispatch loops,
// and drains any queued sessions.
// Safe to call multiple times.
//
// It does not wait for in-flight sessions: the context cancel above is what
// unblocks them, and the server caps its own shutdown separately
// (internal/api/server.go). Use StopAndWait when the caller needs the workers
// to have returned.
func (m *Manager) Stop() {
	m.cancel()
	select {
	case <-m.stopCh:
		// already closed
	default:
		close(m.stopCh)
	}
}

// StopAndWait stops the manager and blocks until every dispatch worker has
// returned, or until timeout elapses. Reports whether the workers finished.
func (m *Manager) StopAndWait(timeout time.Duration) bool {
	m.Stop()
	done := make(chan struct{})
	go func() {
		m.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// RunningCount returns the number of sessions currently executing.
func (m *Manager) RunningCount() int {
	return int(m.running.Load())
}

// MaxConcurrent returns the configured ceiling on simultaneous sessions.
func (m *Manager) MaxConcurrent() int {
	return m.maxConcurrent
}

func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.cleanupExpiredSessions()
		}
	}
}

func (m *Manager) cleanupExpiredSessions() {
	cutoff := time.Now().Add(-time.Duration(m.sessionRetentionSeconds) * time.Second)

	m.mu.Lock()
	defer m.mu.Unlock()

	for id, session := range m.sessions {
		session.mu.RLock()
		completedAt := session.CompletedAt
		session.mu.RUnlock()

		if completedAt != nil && completedAt.Before(cutoff) {
			delete(m.sessions, id)
		}
	}
}

// dispatchLoop is one worker: it pulls an item off the queue and runs it
// synchronously, so each worker holds at most one session at a time. NewManager
// starts maxConcurrent of these, which is what bounds concurrency — the queue
// itself is just a buffer.
func (m *Manager) dispatchLoop() {
	defer m.workers.Done()
	for {
		select {
		case <-m.stopCh:
			// Exactly one worker drains; the rest return immediately. Without
			// the guard, several workers race to pop the same remaining items
			// and can Fail an already-failed session.
			m.drainOnce.Do(m.drainQueue)
			return
		case item := <-m.queue:
			// Transition from queued to running
			item.session.mu.Lock()
			if item.session.Status == SessionStatusQueued {
				item.session.Status = SessionStatusRunning
				item.session.StartedAt = time.Now()
			}
			item.session.mu.Unlock()
			if m.journal != nil {
				m.journal.RecordRunning(item.session.Snapshot())
			}

			// Run synchronously — this worker takes nothing else until it returns
			m.running.Add(1)
			item.startFunc(item.session)
			m.running.Add(-1)
		}
	}
}

// drainQueue fails all remaining queued sessions on shutdown.
func (m *Manager) drainQueue() {
	for {
		select {
		case item := <-m.queue:
			item.session.Fail("agent queue shut down")
			if m.journal != nil {
				m.journal.Remove(item.session.ID)
			}
		default:
			return
		}
	}
}

// Enqueue adds a session to the dispatch queue. Returns an error if the queue is full.
func (m *Manager) Enqueue(session *Session, startFunc func(*Session)) error {
	// Journal before the channel send: once the item is in the queue the
	// dispatch loop may pop it and record "running" immediately — writing
	// "queued" afterwards would overwrite the newer state.
	if m.journal != nil {
		m.journal.RecordQueued(session.Snapshot())
	}
	select {
	case m.queue <- &queueItem{session: session, startFunc: startFunc}:
		// Refresh here too: the execution engine only sets this when a session
		// ends, so without it the gauge reads stale for a whole run.
		metrics.QueueDepth.Set(float64(len(m.queue)))
		return nil
	default:
		if m.journal != nil {
			m.journal.Remove(session.ID)
		}
		return fmt.Errorf("agent queue is full")
	}
}

// QueueLength returns the current number of items waiting in the queue.
func (m *Manager) QueueLength() int {
	return len(m.queue)
}

// LastCompletedSession returns a snapshot of the most recently completed or failed session,
// or nil if no session has finished yet.
func (m *Manager) LastCompletedSession() *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var last *Session
	for _, s := range m.sessions {
		s.mu.RLock()
		status := s.Status
		completedAt := s.CompletedAt
		s.mu.RUnlock()
		if (status == SessionStatusCompleted || status == SessionStatusFailed || status == SessionStatusStopped) && completedAt != nil {
			if last == nil {
				last = s
				continue
			}
			last.mu.RLock()
			lastAt := last.CompletedAt
			last.mu.RUnlock()
			if completedAt.After(*lastAt) {
				last = s
			}
		}
	}
	if last == nil {
		return nil
	}
	return last.Snapshot()
}

// ListActiveSessions returns snapshots of all non-terminal sessions (queued, running, stopping).
func (m *Manager) ListActiveSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var active []*Session
	for _, s := range m.sessions {
		s.mu.RLock()
		status := s.Status
		s.mu.RUnlock()
		if status != SessionStatusCompleted && status != SessionStatusFailed && status != SessionStatusStopped {
			active = append(active, s.Snapshot())
		}
	}
	return active
}

// CreateSession creates a new agent session
func (m *Manager) CreateSession(message string, paths []string, author, commitPrefix string, maxIter, maxSeconds int) (*Session, error) {
	sessionID := "agent-" + uuid.New().String()

	session := &Session{
		ID:                  sessionID,
		Message:             message,
		Paths:               paths,
		Author:              author,
		CommitMessagePrefix: commitPrefix,
		MaxIterations:       maxIter,
		MaxTotalSeconds:     maxSeconds,
		Status:              SessionStatusQueued,
		Iterations:          []IterationResult{},
		StartedAt:           time.Now(),
		notify:              make(chan struct{}, 1),
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	return session, nil
}

// RestoreSession registers a session rebuilt from the journal under its
// original ID (so links/messages shown to the user before the restart stay
// valid). Errors if the ID is already registered. Initializes the notify
// channel — restored sessions must be SSE-subscribable like any other.
func (m *Manager) RestoreSession(session *Session) error {
	if session.ID == "" {
		return fmt.Errorf("restore: session ID is required")
	}
	if session.notify == nil {
		session.notify = make(chan struct{}, 1)
	}
	if session.Iterations == nil {
		session.Iterations = []IterationResult{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[session.ID]; exists {
		return fmt.Errorf("restore: session %s already exists", session.ID)
	}
	m.sessions[session.ID] = session
	return nil
}

// GetSession returns a snapshot of a session by ID
func (m *Manager) GetSession(sessionID string) (*Session, bool) {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return nil, false
	}
	return session.Snapshot(), true
}

// StopSession signals a session to stop after the current iteration
func (m *Manager) StopSession(sessionID string) error {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("session %s not found", sessionID)
	}

	session.RequestStop()
	return nil
}

// CompleteSession marks a session as done
func (m *Manager) CompleteSession(sessionID string) {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return
	}

	session.Complete()
	if m.journal != nil {
		m.journal.Remove(sessionID)
	}
}

// FailSession marks a session as failed
func (m *Manager) FailSession(sessionID, errMsg string) {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return
	}

	session.Fail(errMsg)
	if m.journal != nil {
		m.journal.Remove(sessionID)
	}
}

// MarkSessionStopped marks a session as stopped at the user's request —
// a distinct terminal state from completed/failed (see Session.Stop).
func (m *Manager) MarkSessionStopped(sessionID, reason string) {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return
	}

	session.Stop(reason)
	if m.journal != nil {
		m.journal.Remove(sessionID)
	}
}

// GetSessionDirect returns the live session pointer (for the executor loop to mutate)
func (m *Manager) GetSessionDirect(sessionID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, exists := m.sessions[sessionID]
	return session, exists
}

// ListSessions returns up to limit session snapshots sorted newest-first.
// Pass limit <= 0 for all sessions.
func (m *Manager) ListSessions(limit int) []*Session {
	m.mu.RLock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool {
		all[i].mu.RLock()
		ti := all[i].StartedAt
		all[i].mu.RUnlock()
		all[j].mu.RLock()
		tj := all[j].StartedAt
		all[j].mu.RUnlock()
		return ti.After(tj)
	})

	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}

	result := make([]*Session, len(all))
	for i, s := range all {
		result[i] = s.Snapshot()
	}
	return result
}
