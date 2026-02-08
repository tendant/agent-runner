package jobs

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Status represents the current state of a job
type Status string

const (
	StatusQueued     Status = "queued"
	StatusRunning    Status = "running"
	StatusValidating Status = "validating"
	StatusPushing    Status = "pushing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// DiffSummary contains statistics about changes
type DiffSummary struct {
	Insertions int `json:"insertions"`
	Deletions  int `json:"deletions"`
}

// JobResult contains the result of a completed job
type JobResult struct {
	Commit       string      `json:"commit,omitempty"`
	ChangedFiles []string    `json:"changed_files,omitempty"`
	DiffSummary  DiffSummary `json:"diff_summary,omitempty"`
	LogFile      string      `json:"log_file,omitempty"`
	Duration     int         `json:"duration_seconds,omitempty"`
}

// Job represents an execution job
type Job struct {
	ID          string     `json:"job_id"`
	Project     string     `json:"project"`
	Status      Status     `json:"status"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
	ErrorCode   string     `json:"error_code,omitempty"`
	Result      *JobResult `json:"-"`

	// Internal fields not exposed in JSON
	Instruction   string
	Paths         []string
	CommitMessage string
	Author        string
	WorkspacePath string
}

// ToResponse converts a Job to a response-appropriate format
func (j *Job) ToResponse() map[string]interface{} {
	resp := map[string]interface{}{
		"job_id":  j.ID,
		"status":  j.Status,
		"project": j.Project,
	}

	if j.StartedAt != nil {
		resp["started_at"] = j.StartedAt.Format(time.RFC3339)
		if j.Status == StatusRunning || j.Status == StatusValidating || j.Status == StatusPushing {
			resp["elapsed_seconds"] = int(time.Since(*j.StartedAt).Seconds())
		}
	}

	if j.Error != "" {
		resp["error"] = j.Error
	}
	if j.ErrorCode != "" {
		resp["error_code"] = j.ErrorCode
	}

	if j.Result != nil {
		if j.Result.Commit != "" {
			resp["commit"] = j.Result.Commit
		}
		if len(j.Result.ChangedFiles) > 0 {
			resp["changed_files"] = j.Result.ChangedFiles
		}
		if j.Result.DiffSummary.Insertions > 0 || j.Result.DiffSummary.Deletions > 0 {
			resp["diff_summary"] = j.Result.DiffSummary
		}
		if j.Result.LogFile != "" {
			resp["log_file"] = j.Result.LogFile
		}
		if j.Result.Duration > 0 {
			resp["duration_seconds"] = j.Result.Duration
		}
	}

	return resp
}

// Lock represents a project lock
type Lock struct {
	JobID    string
	LockedAt time.Time
}

// Manager handles job state and project locks
type Manager struct {
	mu                  sync.RWMutex
	jobs                map[string]*Job
	locks               map[string]*Lock
	jobRetentionSeconds int
	maxConcurrentJobs   int
}

// NewManager creates a new job manager
func NewManager(jobRetentionSeconds, maxConcurrentJobs int) *Manager {
	m := &Manager{
		jobs:                make(map[string]*Job),
		locks:               make(map[string]*Lock),
		jobRetentionSeconds: jobRetentionSeconds,
		maxConcurrentJobs:   maxConcurrentJobs,
	}
	go m.cleanupLoop()
	return m
}

// CreateJob creates a new job and returns its ID
func (m *Manager) CreateJob(project, instruction string, paths []string, commitMessage, author string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if project is locked
	if lock, exists := m.locks[project]; exists {
		return nil, fmt.Errorf("project %s is locked by job %s", project, lock.JobID)
	}

	// Check capacity
	runningCount := 0
	for _, job := range m.jobs {
		if job.Status == StatusQueued || job.Status == StatusRunning ||
			job.Status == StatusValidating || job.Status == StatusPushing {
			runningCount++
		}
	}
	if runningCount >= m.maxConcurrentJobs {
		return nil, fmt.Errorf("system at capacity")
	}

	jobID := uuid.New().String()
	job := &Job{
		ID:            jobID,
		Project:       project,
		Status:        StatusQueued,
		Instruction:   instruction,
		Paths:         paths,
		CommitMessage: commitMessage,
		Author:        author,
	}

	m.jobs[jobID] = job
	m.locks[project] = &Lock{
		JobID:    jobID,
		LockedAt: time.Now(),
	}

	return job, nil
}

// GetJob retrieves a snapshot of a job by ID
func (m *Manager) GetJob(jobID string) (*Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, exists := m.jobs[jobID]
	if !exists {
		return nil, false
	}
	// Return a copy to avoid data races with background goroutines
	snapshot := *job
	if job.Result != nil {
		resultCopy := *job.Result
		snapshot.Result = &resultCopy
	}
	return &snapshot, true
}

// UpdateStatus updates the status of a job
func (m *Manager) UpdateStatus(jobID string, status Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job %s not found", jobID)
	}

	job.Status = status
	if status == StatusRunning && job.StartedAt == nil {
		now := time.Now()
		job.StartedAt = &now
	}
	if status == StatusCompleted || status == StatusFailed {
		now := time.Now()
		job.CompletedAt = &now
		// Release lock
		delete(m.locks, job.Project)
	}

	return nil
}

// SetJobError sets an error on a job and marks it as failed
func (m *Manager) SetJobError(jobID, errorMsg, errorCode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job %s not found", jobID)
	}

	job.Error = errorMsg
	job.ErrorCode = errorCode
	job.Status = StatusFailed
	now := time.Now()
	job.CompletedAt = &now
	delete(m.locks, job.Project)

	return nil
}

// SetJobResult sets the result on a completed job
func (m *Manager) SetJobResult(jobID string, result *JobResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job %s not found", jobID)
	}

	job.Result = result
	return nil
}

// SetJobLogFile sets the log file path on a job's result
func (m *Manager) SetJobLogFile(jobID, logFile string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job %s not found", jobID)
	}

	if job.Result != nil {
		job.Result.LogFile = logFile
	}
	return nil
}

// SetWorkspacePath sets the workspace path for a job
func (m *Manager) SetWorkspacePath(jobID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job %s not found", jobID)
	}

	job.WorkspacePath = path
	return nil
}

// IsProjectLocked checks if a project is locked
func (m *Manager) IsProjectLocked(project string) (bool, *Lock) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lock, exists := m.locks[project]
	return exists, lock
}

// GetProjectStatus returns the lock status of a project
func (m *Manager) GetProjectStatus(project string) map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	resp := map[string]interface{}{
		"project": project,
		"locked":  false,
	}

	if lock, exists := m.locks[project]; exists {
		resp["locked"] = true
		resp["current_job_id"] = lock.JobID
		resp["locked_since"] = lock.LockedAt.Format(time.RFC3339)
	}

	return resp
}

// ListProjects returns lock status for multiple projects
func (m *Manager) ListProjects(projects []string) []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(projects))
	for _, project := range projects {
		item := map[string]interface{}{
			"name":   project,
			"locked": false,
		}
		if lock, exists := m.locks[project]; exists {
			item["locked"] = true
			item["current_job_id"] = lock.JobID
		}
		result = append(result, item)
	}
	return result
}

// ReleaseLock manually releases a project lock (for error recovery)
func (m *Manager) ReleaseLock(project string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, project)
}

// cleanupLoop periodically removes old completed jobs
func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		m.cleanupExpiredJobs()
	}
}

func (m *Manager) cleanupExpiredJobs() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(m.jobRetentionSeconds) * time.Second)
	for id, job := range m.jobs {
		if job.CompletedAt != nil && job.CompletedAt.Before(cutoff) {
			delete(m.jobs, id)
		}
	}
}

// AcquireProjectLock acquires a project lock for an external holder (e.g., agent sessions).
// Returns an error if the project is already locked.
func (m *Manager) AcquireProjectLock(project, holderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if lock, exists := m.locks[project]; exists {
		return fmt.Errorf("project %s is locked by %s", project, lock.JobID)
	}

	m.locks[project] = &Lock{
		JobID:    holderID,
		LockedAt: time.Now(),
	}
	return nil
}

// ReleaseProjectLock releases a project lock held by an external holder.
func (m *Manager) ReleaseProjectLock(project, holderID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if lock, exists := m.locks[project]; exists && lock.JobID == holderID {
		delete(m.locks, project)
	}
}

// GetRunningJobIDs returns IDs of all currently running jobs (for startup cleanup)
func (m *Manager) GetRunningJobIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var ids []string
	for id, job := range m.jobs {
		if job.Status != StatusCompleted && job.Status != StatusFailed {
			ids = append(ids, id)
		}
	}
	return ids
}
