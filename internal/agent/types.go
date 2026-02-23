package agent

import (
	"sync"
	"time"
)

// SessionStatus represents the current state of an agent session
type SessionStatus string

const (
	SessionStatusQueued    SessionStatus = "queued"
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

// OutputFile represents a file the agent wants to send back to the user.
type OutputFile struct {
	Name        string `json:"name"`
	Data        []byte `json:"-"`
	ContentType string `json:"content_type"`
}

// IterationResult captures the outcome of one iteration
type IterationResult struct {
	Iteration    int             `json:"iteration"`
	Status       IterationStatus `json:"status"`
	Retry        bool            `json:"retry,omitempty"` // true if this iteration retried after a previous failure
	Commit       string          `json:"commit,omitempty"`
	ChangedFiles []string        `json:"changed_files,omitempty"`
	Output       string          `json:"output,omitempty"` // Claude's result text
	Error        string          `json:"error,omitempty"`
	DurationSecs int             `json:"duration_seconds"`
	Prompt       string          `json:"-"` // prompt sent to Claude (excluded from API response)
}

// Session represents an agent session that runs multiple iterations
type Session struct {
	mu sync.RWMutex

	ID                   string            `json:"session_id"`
	Message              string            `json:"message"`
	Paths                []string          `json:"paths"`
	Author               string            `json:"author"`
	CommitMessagePrefix  string            `json:"commit_message_prefix"`
	MaxIterations        int               `json:"max_iterations"`
	MaxTotalSeconds      int               `json:"max_total_seconds"`
	Status               SessionStatus     `json:"status"`
	CurrentIteration     int               `json:"current_iteration"`
	SuccessfulIterations int               `json:"successful_iterations"`
	Iterations           []IterationResult `json:"iterations"`
	StartedAt            time.Time         `json:"started_at"`
	CompletedAt          *time.Time        `json:"completed_at,omitempty"`
	Error                string            `json:"error,omitempty"`
	WorkspacePath        string            `json:"-"`
	ElapsedSeconds       int               `json:"elapsed_seconds"`
	ConsecutiveFailures  int               `json:"-"`
	OutputFiles          []OutputFile      `json:"-"`
	CompletedSteps       []string          `json:"-"`
	PlanJSON             any               `json:"-"`
	ReviewJSON           any               `json:"-"`

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
		s.SuccessfulIterations++
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

// SetWorkspacePath stores the workspace path on the session.
func (s *Session) SetWorkspacePath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WorkspacePath = path
}

// GetConsecutiveFailures returns the current consecutive failure count
func (s *Session) GetConsecutiveFailures() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ConsecutiveFailures
}

// SetPlanResult stores the planner output on the session.
func (s *Session) SetPlanResult(plan any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PlanJSON = plan
}

// SetReviewResult stores the reviewer output on the session.
func (s *Session) SetReviewResult(review any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReviewJSON = review
}

// SetCompletedSteps updates the list of completed plan step IDs.
func (s *Session) SetCompletedSteps(steps []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CompletedSteps = steps
}

// SetOutputFiles stores files collected from the _send/ directory.
func (s *Session) SetOutputFiles(files []OutputFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.OutputFiles = files
}

// LastIterationError returns details of the most recent iteration if it was
// a failure. Returns zero values if there are no iterations or the last one
// succeeded.
func (s *Session) LastIterationError() (iterNum int, errMsg string, partialOutput string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.Iterations) == 0 {
		return 0, "", ""
	}
	last := s.Iterations[len(s.Iterations)-1]
	if last.Status != IterationStatusError && last.Status != IterationStatusValidation {
		return 0, "", ""
	}
	return last.Iteration, last.Error, last.Output
}

// Snapshot returns a deep copy of the session for safe concurrent reading
func (s *Session) Snapshot() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := &Session{
		ID:                  s.ID,
		Message:             s.Message,
		Paths:               append([]string{}, s.Paths...),
		Author:              s.Author,
		CommitMessagePrefix: s.CommitMessagePrefix,
		MaxIterations:       s.MaxIterations,
		MaxTotalSeconds:     s.MaxTotalSeconds,
		Status:              s.Status,
		CurrentIteration:    s.CurrentIteration,
		SuccessfulIterations: s.SuccessfulIterations,
		Iterations:          make([]IterationResult, len(s.Iterations)),
		StartedAt:           s.StartedAt,
		CompletedAt:         s.CompletedAt,
		Error:               s.Error,
		ElapsedSeconds:      int(time.Since(s.StartedAt).Seconds()),
		CompletedSteps:      append([]string{}, s.CompletedSteps...),
		PlanJSON:            s.PlanJSON,
		ReviewJSON:          s.ReviewJSON,
	}
	copy(snap.Iterations, s.Iterations)

	if len(s.OutputFiles) > 0 {
		snap.OutputFiles = make([]OutputFile, len(s.OutputFiles))
		for i, f := range s.OutputFiles {
			dataCopy := make([]byte, len(f.Data))
			copy(dataCopy, f.Data)
			snap.OutputFiles[i] = OutputFile{
				Name:        f.Name,
				Data:        dataCopy,
				ContentType: f.ContentType,
			}
		}
	}

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
		"status":            s.Status,
		"current_iteration": s.CurrentIteration,
		"successful_iterations": s.SuccessfulIterations,
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
	if len(s.CompletedSteps) > 0 {
		resp["completed_steps"] = s.CompletedSteps
	}
	if s.PlanJSON != nil {
		resp["plan"] = s.PlanJSON
	}
	if s.ReviewJSON != nil {
		resp["review"] = s.ReviewJSON
	}

	return resp
}
