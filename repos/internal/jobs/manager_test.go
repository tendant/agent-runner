package jobs

import (
	"sync"
	"testing"
	"time"
)

func newTestManager() *Manager {
	return &Manager{
		jobs:                make(map[string]*Job),
		locks:               make(map[string]*Lock),
		jobRetentionSeconds: 3600,
		maxConcurrentJobs:   5,
	}
}

func TestCreateJob_Success(t *testing.T) {
	m := newTestManager()

	job, err := m.CreateJob("test-project", "fix bug", []string{"src/"}, "", "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}
	if job.Project != "test-project" {
		t.Errorf("expected project test-project, got %s", job.Project)
	}
	if job.Status != StatusQueued {
		t.Errorf("expected status queued, got %s", job.Status)
	}
	if job.Instruction != "fix bug" {
		t.Errorf("expected instruction 'fix bug', got %s", job.Instruction)
	}
	if job.Author != "alice" {
		t.Errorf("expected author alice, got %s", job.Author)
	}
}

func TestCreateJob_LocksProject(t *testing.T) {
	m := newTestManager()

	_, err := m.CreateJob("test-project", "task 1", []string{"src/"}, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = m.CreateJob("test-project", "task 2", []string{"src/"}, "", "")
	if err == nil {
		t.Error("expected error for locked project")
	}
}

func TestCreateJob_DifferentProjectsOK(t *testing.T) {
	m := newTestManager()

	_, err := m.CreateJob("project-a", "task 1", []string{"src/"}, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = m.CreateJob("project-b", "task 2", []string{"src/"}, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateJob_CapacityCheck(t *testing.T) {
	m := newTestManager()
	m.maxConcurrentJobs = 2

	_, err := m.CreateJob("project-a", "t", []string{"src/"}, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = m.CreateJob("project-b", "t", []string{"src/"}, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = m.CreateJob("project-c", "t", []string{"src/"}, "", "")
	if err == nil {
		t.Error("expected capacity error")
	}
}

func TestGetJob_Found(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	got, exists := m.GetJob(job.ID)
	if !exists {
		t.Error("expected job to exist")
	}
	if got.ID != job.ID {
		t.Errorf("expected %s, got %s", job.ID, got.ID)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	m := newTestManager()

	_, exists := m.GetJob("nonexistent")
	if exists {
		t.Error("expected job to not exist")
	}
}

func TestUpdateStatus_SetsStartedAt(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	if job.StartedAt != nil {
		t.Error("StartedAt should be nil initially")
	}

	if err := m.UpdateStatus(job.ID, StatusRunning); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := m.GetJob(job.ID)
	if got.StartedAt == nil {
		t.Error("expected StartedAt to be set")
	}
	if got.Status != StatusRunning {
		t.Errorf("expected running, got %s", got.Status)
	}
}

func TestUpdateStatus_ReleasesLockOnCompleted(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	locked, _ := m.IsProjectLocked("test")
	if !locked {
		t.Error("expected project to be locked")
	}

	m.UpdateStatus(job.ID, StatusCompleted)

	locked, _ = m.IsProjectLocked("test")
	if locked {
		t.Error("expected lock to be released on completed")
	}
}

func TestUpdateStatus_ReleasesLockOnFailed(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	m.UpdateStatus(job.ID, StatusFailed)

	locked, _ := m.IsProjectLocked("test")
	if locked {
		t.Error("expected lock to be released on failed")
	}
}

func TestUpdateStatus_SetsCompletedAt(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	m.UpdateStatus(job.ID, StatusCompleted)

	got, _ := m.GetJob(job.ID)
	if got.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}
}

func TestUpdateStatus_NotFound(t *testing.T) {
	m := newTestManager()
	if err := m.UpdateStatus("nonexistent", StatusRunning); err == nil {
		t.Error("expected error for nonexistent job")
	}
}

func TestSetJobError(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	if err := m.SetJobError(job.ID, "something broke", "CLAUDE_ERROR"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := m.GetJob(job.ID)
	if got.Error != "something broke" {
		t.Errorf("expected error message 'something broke', got %s", got.Error)
	}
	if got.ErrorCode != "CLAUDE_ERROR" {
		t.Errorf("expected error code CLAUDE_ERROR, got %s", got.ErrorCode)
	}
	if got.Status != StatusFailed {
		t.Errorf("expected failed status, got %s", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	// Lock should be released
	locked, _ := m.IsProjectLocked("test")
	if locked {
		t.Error("expected lock to be released on error")
	}
}

func TestSetJobError_NotFound(t *testing.T) {
	m := newTestManager()
	if err := m.SetJobError("nonexistent", "err", "code"); err == nil {
		t.Error("expected error for nonexistent job")
	}
}

func TestSetJobResult(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	result := &JobResult{
		Commit:       "abc1234",
		ChangedFiles: []string{"main.go"},
		DiffSummary:  DiffSummary{Insertions: 10, Deletions: 5},
		Duration:     42,
	}

	if err := m.SetJobResult(job.ID, result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := m.GetJob(job.ID)
	if got.Result == nil {
		t.Fatal("expected result to be set")
	}
	if got.Result.Commit != "abc1234" {
		t.Errorf("expected commit abc1234, got %s", got.Result.Commit)
	}
}

func TestSetJobResult_NotFound(t *testing.T) {
	m := newTestManager()
	if err := m.SetJobResult("nonexistent", &JobResult{}); err == nil {
		t.Error("expected error for nonexistent job")
	}
}

func TestSetWorkspacePath(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	if err := m.SetWorkspacePath(job.ID, "/tmp/workspace"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := m.GetJob(job.ID)
	if got.WorkspacePath != "/tmp/workspace" {
		t.Errorf("expected /tmp/workspace, got %s", got.WorkspacePath)
	}
}

func TestSetWorkspacePath_NotFound(t *testing.T) {
	m := newTestManager()
	if err := m.SetWorkspacePath("nonexistent", "/tmp"); err == nil {
		t.Error("expected error for nonexistent job")
	}
}

func TestIsProjectLocked(t *testing.T) {
	m := newTestManager()

	locked, lock := m.IsProjectLocked("test")
	if locked {
		t.Error("expected not locked initially")
	}
	if lock != nil {
		t.Error("expected nil lock")
	}

	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")

	locked, lock = m.IsProjectLocked("test")
	if !locked {
		t.Error("expected locked after job creation")
	}
	if lock.JobID != job.ID {
		t.Errorf("expected job ID %s, got %s", job.ID, lock.JobID)
	}
}

func TestGetProjectStatus(t *testing.T) {
	m := newTestManager()

	// Unlocked
	status := m.GetProjectStatus("test")
	if status["locked"] != false {
		t.Error("expected locked=false")
	}

	// Locked
	job, _ := m.CreateJob("test", "t", []string{"src/"}, "", "")
	status = m.GetProjectStatus("test")
	if status["locked"] != true {
		t.Error("expected locked=true")
	}
	if status["current_job_id"] != job.ID {
		t.Errorf("expected job ID %s", job.ID)
	}
}

func TestListProjects(t *testing.T) {
	m := newTestManager()
	job, _ := m.CreateJob("project-a", "t", []string{"src/"}, "", "")

	result := m.ListProjects([]string{"project-a", "project-b"})
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}

	// project-a should be locked
	if result[0]["name"] != "project-a" {
		t.Errorf("expected project-a, got %s", result[0]["name"])
	}
	if result[0]["locked"] != true {
		t.Error("expected project-a to be locked")
	}
	if result[0]["current_job_id"] != job.ID {
		t.Errorf("expected job ID %s", job.ID)
	}

	// project-b should be unlocked
	if result[1]["locked"] != false {
		t.Error("expected project-b to be unlocked")
	}
}

func TestToResponse_Queued(t *testing.T) {
	job := &Job{
		ID:      "job-1",
		Project: "test",
		Status:  StatusQueued,
	}

	resp := job.ToResponse()
	if resp["job_id"] != "job-1" {
		t.Errorf("expected job-1, got %v", resp["job_id"])
	}
	if resp["status"] != StatusQueued {
		t.Errorf("expected queued, got %v", resp["status"])
	}
	if _, ok := resp["elapsed_seconds"]; ok {
		t.Error("queued job should not have elapsed_seconds")
	}
}

func TestToResponse_Running(t *testing.T) {
	now := time.Now().Add(-10 * time.Second)
	job := &Job{
		ID:        "job-1",
		Project:   "test",
		Status:    StatusRunning,
		StartedAt: &now,
	}

	resp := job.ToResponse()
	elapsed, ok := resp["elapsed_seconds"]
	if !ok {
		t.Fatal("running job should have elapsed_seconds")
	}
	if elapsed.(int) < 9 {
		t.Errorf("expected elapsed >= 9, got %d", elapsed.(int))
	}
}

func TestToResponse_Completed(t *testing.T) {
	now := time.Now()
	job := &Job{
		ID:        "job-1",
		Project:   "test",
		Status:    StatusCompleted,
		StartedAt: &now,
		Result: &JobResult{
			Commit:       "abc123",
			ChangedFiles: []string{"main.go"},
			DiffSummary:  DiffSummary{Insertions: 10, Deletions: 5},
			LogFile:      "/runs/log.md",
			Duration:     42,
		},
	}

	resp := job.ToResponse()
	if resp["commit"] != "abc123" {
		t.Errorf("expected commit abc123, got %v", resp["commit"])
	}
	if resp["duration_seconds"] != 42 {
		t.Errorf("expected duration 42, got %v", resp["duration_seconds"])
	}
	if resp["log_file"] != "/runs/log.md" {
		t.Errorf("expected log file, got %v", resp["log_file"])
	}

	files, ok := resp["changed_files"].([]string)
	if !ok {
		t.Fatal("expected changed_files to be []string")
	}
	if len(files) != 1 || files[0] != "main.go" {
		t.Error("unexpected changed_files")
	}
}

func TestToResponse_Failed(t *testing.T) {
	job := &Job{
		ID:        "job-1",
		Project:   "test",
		Status:    StatusFailed,
		Error:     "something broke",
		ErrorCode: "CLAUDE_ERROR",
	}

	resp := job.ToResponse()
	if resp["error"] != "something broke" {
		t.Errorf("expected error message, got %v", resp["error"])
	}
	if resp["error_code"] != "CLAUDE_ERROR" {
		t.Errorf("expected error code, got %v", resp["error_code"])
	}
}

func TestToResponse_NilResult(t *testing.T) {
	job := &Job{
		ID:      "job-1",
		Project: "test",
		Status:  StatusCompleted,
	}

	resp := job.ToResponse()
	if _, ok := resp["commit"]; ok {
		t.Error("nil result should not include commit")
	}
}

func TestToResponse_ZeroDiffSummary(t *testing.T) {
	job := &Job{
		ID:      "job-1",
		Project: "test",
		Status:  StatusCompleted,
		Result: &JobResult{
			Commit:      "abc",
			DiffSummary: DiffSummary{Insertions: 0, Deletions: 0},
		},
	}

	resp := job.ToResponse()
	if _, ok := resp["diff_summary"]; ok {
		t.Error("zero diff summary should not be included")
	}
}

func TestCleanupExpiredJobs(t *testing.T) {
	m := newTestManager()
	m.jobRetentionSeconds = 60

	// Old completed job
	oldTime := time.Now().Add(-2 * time.Hour)
	m.jobs["old-job"] = &Job{
		ID:          "old-job",
		Status:      StatusCompleted,
		CompletedAt: &oldTime,
	}

	// Recent completed job
	recentTime := time.Now().Add(-30 * time.Second)
	m.jobs["recent-job"] = &Job{
		ID:          "recent-job",
		Status:      StatusCompleted,
		CompletedAt: &recentTime,
	}

	// Running job (no CompletedAt)
	m.jobs["running-job"] = &Job{
		ID:     "running-job",
		Status: StatusRunning,
	}

	m.cleanupExpiredJobs()

	if _, exists := m.jobs["old-job"]; exists {
		t.Error("expected old completed job to be cleaned up")
	}
	if _, exists := m.jobs["recent-job"]; !exists {
		t.Error("expected recent job to be kept")
	}
	if _, exists := m.jobs["running-job"]; !exists {
		t.Error("expected running job to be kept")
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := newTestManager()
	m.maxConcurrentJobs = 100

	var wg sync.WaitGroup
	const goroutines = 20

	// Concurrently create jobs for different projects
	wg.Add(goroutines)
	jobIDs := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			project := "project-" + string(rune('a'+idx))
			job, err := m.CreateJob(project, "task", []string{"src/"}, "", "")
			if err != nil {
				return
			}
			jobIDs <- job.ID
		}(i)
	}
	wg.Wait()
	close(jobIDs)

	// Collect job IDs
	var ids []string
	for id := range jobIDs {
		ids = append(ids, id)
	}

	// Concurrently read and update
	wg.Add(len(ids) * 2)
	for _, id := range ids {
		go func(jobID string) {
			defer wg.Done()
			m.GetJob(jobID)
		}(id)
		go func(jobID string) {
			defer wg.Done()
			m.UpdateStatus(jobID, StatusRunning)
		}(id)
	}
	wg.Wait()
}
