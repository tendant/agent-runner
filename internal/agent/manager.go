package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ProjectLocker is the interface for project locking (satisfied by jobs.Manager)
type ProjectLocker interface {
	AcquireProjectLock(project, holderID string) error
	ReleaseProjectLock(project, holderID string)
}

// Manager manages agent sessions
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	locker   ProjectLocker
}

// NewManager creates a new agent session manager
func NewManager(locker ProjectLocker) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		locker:   locker,
	}
}

// CreateSession creates a new agent session and acquires the project lock
func (m *Manager) CreateSession(project, message string, paths []string, author, commitPrefix string, maxIter, maxSeconds int) (*Session, error) {
	sessionID := "agent-" + uuid.New().String()

	// Acquire project lock
	if err := m.locker.AcquireProjectLock(project, sessionID); err != nil {
		return nil, fmt.Errorf("project %s is locked: %w", project, err)
	}

	session := &Session{
		ID:                  sessionID,
		Project:             project,
		Message:             message,
		Paths:               paths,
		Author:              author,
		CommitMessagePrefix: commitPrefix,
		MaxIterations:       maxIter,
		MaxTotalSeconds:     maxSeconds,
		Status:              SessionStatusRunning,
		Iterations:          []IterationResult{},
		StartedAt:           time.Now(),
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	return session, nil
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

// CompleteSession marks a session as done and releases the project lock
func (m *Manager) CompleteSession(sessionID string) {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return
	}

	session.Complete()
	m.locker.ReleaseProjectLock(session.Project, sessionID)
}

// FailSession marks a session as failed and releases the project lock
func (m *Manager) FailSession(sessionID, errMsg string) {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return
	}

	session.Fail(errMsg)
	m.locker.ReleaseProjectLock(session.Project, sessionID)
}

// GetSessionDirect returns the live session pointer (for the executor loop to mutate)
func (m *Manager) GetSessionDirect(sessionID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, exists := m.sessions[sessionID]
	return session, exists
}
