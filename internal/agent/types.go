package agent

import (
	"sync"
	"time"
)

// SessionStatus represents the current state of an agent session
type SessionStatus string

const (
	SessionStatusRunning   SessionStatus = "running"
	SessionStatusStopping  SessionStatus = "stopping"
	SessionStatusCompleted SessionStatus = "completed"
	SessionStatusFailed    SessionStatus = "failed"
)

// IterationStatus represents the outcome of a single iteration
type IterationStatus string

const (
	IterationStatusSuccess    IterationStatus = "success"
	IterationStatusNoChanges  IterationStatus = "no_changes"
	IterationStatusValidation IterationStatus = "validation_failed"
	IterationStatusError      IterationStatus = "error"
)

// IterationResult captures the outcome of one iteration
type IterationResult struct {
	Iteration    int             `json:"iteration"`
	Status       IterationStatus `json:"status"`
	Commit       string          `json:"commit,omitempty"`
	ChangedFiles []string        `json:"changed_files,omitempty"`
	Error        string          `json:"error,omitempty"`
	DurationSecs int             `json:"duration_seconds"`
}

// Session represents an agent session that runs multiple iterations
type Session struct {
	mu sync.RWMutex

	ID                   string            `json:"session_id"`
	Project              string            `json:"project"`
	PromptFile           string            `json:"prompt_file"`
	Paths                []string          `json:"paths"`
	Author               string            `json:"author"`
	CommitMessagePrefix  string            `json:"commit_message_prefix"`
	MaxIterations        int               `json:"max_iterations"`
	MaxTotalSeconds      int               `json:"max_total_seconds"`
	Status               SessionStatus     `json:"status"`
	CurrentIteration     int               `json:"current_iteration"`
	TotalCommits         int               `json:"total_commits"`
	Iterations           []IterationResult `json:"iterations"`
	StartedAt            time.Time         `json:"started_at"`
	CompletedAt          *time.Time        `json:"completed_at,omitempty"`
	Error                string            `json:"error,omitempty"`
	WorkspacePath        string            `json:"-"`
	ElapsedSeconds       int               `json:"elapsed_seconds"`
	ConsecutiveFailures  int               `json:"-"`

	stopRequested bool
}

// RequestStop sets the stop flag so the loop exits after the current iteration
func (s *Session) RequestStop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopRequested = true
	if s.Status == SessionStatusRunning {
		s.Status = SessionStatusStopping
	}
}

// StopRequested returns true if a stop has been requested
func (s *Session) StopRequested() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stopRequested
}

// AddIteration appends an iteration result and updates counters
func (s *Session) AddIteration(result IterationResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Iterations = append(s.Iterations, result)
	s.CurrentIteration = result.Iteration

	if result.Status == IterationStatusSuccess {
		s.TotalCommits++
		s.ConsecutiveFailures = 0
	} else if result.Status == IterationStatusNoChanges {
		s.ConsecutiveFailures = 0
	} else {
		s.ConsecutiveFailures++
	}
}

// Complete marks the session as completed
func (s *Session) Complete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.CompletedAt = &now
	s.Status = SessionStatusCompleted
	s.ElapsedSeconds = int(now.Sub(s.StartedAt).Seconds())
}

// Fail marks the session as failed with an error
func (s *Session) Fail(err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.CompletedAt = &now
	s.Status = SessionStatusFailed
	s.Error = err
	s.ElapsedSeconds = int(now.Sub(s.StartedAt).Seconds())
}

// GetConsecutiveFailures returns the current consecutive failure count
func (s *Session) GetConsecutiveFailures() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ConsecutiveFailures
}

// Snapshot returns a deep copy of the session for safe concurrent reading
func (s *Session) Snapshot() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := &Session{
		ID:                  s.ID,
		Project:             s.Project,
		PromptFile:          s.PromptFile,
		Paths:               append([]string{}, s.Paths...),
		Author:              s.Author,
		CommitMessagePrefix: s.CommitMessagePrefix,
		MaxIterations:       s.MaxIterations,
		MaxTotalSeconds:     s.MaxTotalSeconds,
		Status:              s.Status,
		CurrentIteration:    s.CurrentIteration,
		TotalCommits:        s.TotalCommits,
		Iterations:          make([]IterationResult, len(s.Iterations)),
		StartedAt:           s.StartedAt,
		CompletedAt:         s.CompletedAt,
		Error:               s.Error,
		ElapsedSeconds:      int(time.Since(s.StartedAt).Seconds()),
	}
	copy(snap.Iterations, s.Iterations)

	if s.CompletedAt != nil {
		t := *s.CompletedAt
		snap.CompletedAt = &t
		snap.ElapsedSeconds = int(t.Sub(s.StartedAt).Seconds())
	}

	return snap
}

// ToResponse converts a session snapshot to a response map
func (s *Session) ToResponse() map[string]any {
	resp := map[string]any{
		"session_id":        s.ID,
		"project":           s.Project,
		"status":            s.Status,
		"current_iteration": s.CurrentIteration,
		"total_commits":     s.TotalCommits,
		"max_iterations":    s.MaxIterations,
		"elapsed_seconds":   s.ElapsedSeconds,
		"started_at":        s.StartedAt.Format(time.RFC3339),
	}

	if s.CompletedAt != nil {
		resp["completed_at"] = s.CompletedAt.Format(time.RFC3339)
	}
	if s.Error != "" {
		resp["error"] = s.Error
	}
	if len(s.Iterations) > 0 {
		resp["iterations"] = s.Iterations
	}

	return resp
}
