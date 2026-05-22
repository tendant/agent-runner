package agent

import (
	"testing"
	"time"
)

func TestLastIterationError_NoIterations(t *testing.T) {
	s := &Session{}
	iterNum, errMsg, partialOut := s.LastIterationError()
	if iterNum != 0 || errMsg != "" || partialOut != "" {
		t.Errorf("expected zero values, got (%d, %q, %q)", iterNum, errMsg, partialOut)
	}
}

func TestLastIterationError_LastSuccess(t *testing.T) {
	s := &Session{}
	s.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusError,
		Error:     "something failed",
		Output:    "partial",
	})
	s.AddIteration(IterationResult{
		Iteration: 2,
		Status:    IterationStatusSuccess,
		Output:    "all good",
	})

	iterNum, errMsg, partialOut := s.LastIterationError()
	if iterNum != 0 || errMsg != "" || partialOut != "" {
		t.Errorf("expected zero values after success, got (%d, %q, %q)", iterNum, errMsg, partialOut)
	}
}

func TestLastIterationError_LastError(t *testing.T) {
	s := &Session{}
	s.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusSuccess,
	})
	s.AddIteration(IterationResult{
		Iteration: 2,
		Status:    IterationStatusError,
		Error:     "claude execution failed: timeout",
		Output:    "partial work here",
	})

	iterNum, errMsg, partialOut := s.LastIterationError()
	if iterNum != 2 {
		t.Errorf("expected iteration 2, got %d", iterNum)
	}
	if errMsg != "claude execution failed: timeout" {
		t.Errorf("unexpected error message: %q", errMsg)
	}
	if partialOut != "partial work here" {
		t.Errorf("unexpected partial output: %q", partialOut)
	}
}

func TestLastIterationError_ValidationFailure(t *testing.T) {
	s := &Session{}
	s.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusValidation,
		Error:     "validation failed",
	})

	iterNum, errMsg, _ := s.LastIterationError()
	if iterNum != 1 || errMsg != "validation failed" {
		t.Errorf("expected validation failure returned, got (%d, %q)", iterNum, errMsg)
	}
}

func TestSetCompletedSteps(t *testing.T) {
	s := &Session{
		ID:        "test-1",
		StartedAt: time.Now(),
	}

	s.SetCompletedSteps([]string{"1", "3"})

	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.CompletedSteps) != 2 {
		t.Fatalf("expected 2 completed steps, got %d", len(s.CompletedSteps))
	}
	if s.CompletedSteps[0] != "1" || s.CompletedSteps[1] != "3" {
		t.Errorf("unexpected steps: %v", s.CompletedSteps)
	}
}

func TestSnapshot_IncludesCompletedSteps(t *testing.T) {
	s := &Session{
		ID:        "test-2",
		StartedAt: time.Now(),
		Status:    SessionStatusRunning,
	}

	s.SetCompletedSteps([]string{"1", "2", "3"})
	snap := s.Snapshot()

	if len(snap.CompletedSteps) != 3 {
		t.Fatalf("expected 3 completed steps in snapshot, got %d", len(snap.CompletedSteps))
	}

	// Verify deep copy — mutating original doesn't affect snapshot
	s.SetCompletedSteps([]string{"1"})
	if len(snap.CompletedSteps) != 3 {
		t.Error("snapshot was mutated after changing original")
	}
}

func TestToResponse_IncludesCompletedSteps(t *testing.T) {
	s := &Session{
		ID:             "test-3",
		StartedAt:      time.Now(),
		Status:         SessionStatusCompleted,
		CompletedSteps: []string{"1", "2"},
	}

	resp := s.ToResponse()
	steps, ok := resp["completed_steps"].([]string)
	if !ok {
		t.Fatal("expected completed_steps in response")
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
}

func TestToResponse_OmitsEmptyCompletedSteps(t *testing.T) {
	s := &Session{
		ID:        "test-4",
		StartedAt: time.Now(),
		Status:    SessionStatusCompleted,
	}

	resp := s.ToResponse()
	if _, ok := resp["completed_steps"]; ok {
		t.Error("expected completed_steps to be omitted when empty")
	}
}

// [C3/M7/M8] Warnings field tests

func TestAddWarning_AppendsWarnings(t *testing.T) {
	s := &Session{ID: "warn-1", StartedAt: time.Now()}
	s.AddWarning("first warning")
	s.AddWarning("second warning")

	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.Warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(s.Warnings))
	}
	if s.Warnings[0] != "first warning" || s.Warnings[1] != "second warning" {
		t.Errorf("unexpected warnings: %v", s.Warnings)
	}
}

func TestSnapshot_IncludesWarnings(t *testing.T) {
	s := &Session{ID: "warn-2", StartedAt: time.Now(), Status: SessionStatusRunning}
	s.AddWarning("memory push failed: timeout")

	snap := s.Snapshot()
	if len(snap.Warnings) != 1 || snap.Warnings[0] != "memory push failed: timeout" {
		t.Errorf("expected 1 warning in snapshot, got %v", snap.Warnings)
	}

	// Deep copy: mutating original must not affect the snapshot.
	s.AddWarning("another warning")
	if len(snap.Warnings) != 1 {
		t.Error("snapshot warnings were mutated after adding to original")
	}
}

func TestToResponse_IncludesWarnings(t *testing.T) {
	s := &Session{
		ID:        "warn-3",
		StartedAt: time.Now(),
		Status:    SessionStatusCompleted,
		Warnings:  []string{"shared repo not found in cache: myrepo"},
	}

	resp := s.ToResponse()
	warnings, ok := resp["warnings"].([]string)
	if !ok {
		t.Fatal("expected warnings key in response")
	}
	if len(warnings) != 1 || warnings[0] != "shared repo not found in cache: myrepo" {
		t.Errorf("unexpected warnings in response: %v", warnings)
	}
}

func TestToResponse_OmitsEmptyWarnings(t *testing.T) {
	s := &Session{
		ID:        "warn-4",
		StartedAt: time.Now(),
		Status:    SessionStatusCompleted,
	}

	resp := s.ToResponse()
	if _, ok := resp["warnings"]; ok {
		t.Error("expected warnings to be omitted when empty")
	}
}
