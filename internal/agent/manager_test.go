package agent

import (
	"sync"
	"testing"
	"time"
)

func newTestManager(t *testing.T, retentionSeconds int) *Manager {
	t.Helper()
	mgr := NewManager(retentionSeconds, 10)
	t.Cleanup(mgr.Stop)
	return mgr
}

func TestCreateSession_Success(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, err := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.ID == "" {
		t.Error("expected non-empty session ID")
	}
	if session.Status != SessionStatusQueued {
		t.Errorf("expected status queued, got %s", session.Status)
	}
	if session.MaxIterations != 10 {
		t.Errorf("expected max_iterations 10, got %d", session.MaxIterations)
	}
}

func TestCreateSession_StatusQueued(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, err := mgr.CreateSession("test", nil, "", "", 1, 60)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.Status != SessionStatusQueued {
		t.Errorf("expected status queued, got %s", session.Status)
	}

	snap, _ := mgr.GetSession(session.ID)
	if snap.Status != SessionStatusQueued {
		t.Errorf("expected snapshot status queued, got %s", snap.Status)
	}
}

func TestGetSession_Exists(t *testing.T) {
	mgr := newTestManager(t, 3600)

	created, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)

	snap, exists := mgr.GetSession(created.ID)
	if !exists {
		t.Fatal("expected session to exist")
	}
	if snap.ID != created.ID {
		t.Errorf("expected ID %s, got %s", created.ID, snap.ID)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	mgr := newTestManager(t, 3600)

	_, exists := mgr.GetSession("nonexistent")
	if exists {
		t.Error("expected session to not exist")
	}
}

func TestStopSession(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)

	// Manually set to running so RequestStop transitions to stopping
	session.mu.Lock()
	session.Status = SessionStatusRunning
	session.mu.Unlock()

	if err := mgr.StopSession(session.ID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snap, _ := mgr.GetSession(session.ID)
	if snap.Status != SessionStatusStopping {
		t.Errorf("expected status stopping, got %s", snap.Status)
	}
}

func TestStopSession_NotFound(t *testing.T) {
	mgr := newTestManager(t, 3600)

	if err := mgr.StopSession("nonexistent"); err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestCompleteSession(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)
	mgr.CompleteSession(session.ID)

	snap, _ := mgr.GetSession(session.ID)
	if snap.Status != SessionStatusCompleted {
		t.Errorf("expected completed, got %s", snap.Status)
	}
	if snap.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}
}

func TestFailSession(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)
	mgr.FailSession(session.ID, "something went wrong")

	snap, _ := mgr.GetSession(session.ID)
	if snap.Status != SessionStatusFailed {
		t.Errorf("expected failed, got %s", snap.Status)
	}
	if snap.Error != "something went wrong" {
		t.Errorf("expected error message, got '%s'", snap.Error)
	}
}

func TestSession_AddIteration(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)

	// Use direct session for mutations
	live, _ := mgr.GetSessionDirect(session.ID)

	live.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusSuccess,
		Commit:    "abc123",
	})

	snap := live.Snapshot()
	if snap.CurrentIteration != 1 {
		t.Errorf("expected iteration 1, got %d", snap.CurrentIteration)
	}
	if snap.SuccessfulIterations != 1 {
		t.Errorf("expected 1 successful iteration, got %d", snap.SuccessfulIterations)
	}
	if len(snap.Iterations) != 1 {
		t.Fatalf("expected 1 iteration, got %d", len(snap.Iterations))
	}
	if snap.Iterations[0].Commit != "abc123" {
		t.Errorf("expected commit abc123, got %s", snap.Iterations[0].Commit)
	}
}

func TestSession_ConsecutiveFailures(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)
	live, _ := mgr.GetSessionDirect(session.ID)

	// Success resets counter
	live.AddIteration(IterationResult{Iteration: 1, Status: IterationStatusSuccess})
	if live.GetConsecutiveFailures() != 0 {
		t.Error("expected 0 consecutive failures after success")
	}

	// Errors increment counter
	live.AddIteration(IterationResult{Iteration: 2, Status: IterationStatusError})
	live.AddIteration(IterationResult{Iteration: 3, Status: IterationStatusError})
	if live.GetConsecutiveFailures() != 2 {
		t.Errorf("expected 2 consecutive failures, got %d", live.GetConsecutiveFailures())
	}

	// No changes resets counter
	live.AddIteration(IterationResult{Iteration: 4, Status: IterationStatusNoChanges})
	if live.GetConsecutiveFailures() != 0 {
		t.Error("expected 0 consecutive failures after no_changes")
	}

	// Validation failures count
	live.AddIteration(IterationResult{Iteration: 5, Status: IterationStatusValidation})
	if live.GetConsecutiveFailures() != 1 {
		t.Errorf("expected 1 consecutive failure, got %d", live.GetConsecutiveFailures())
	}
}

func TestSession_ToResponse(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)
	live, _ := mgr.GetSessionDirect(session.ID)

	live.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusSuccess,
		Commit:    "abc123",
	})

	snap := live.Snapshot()
	resp := snap.ToResponse()

	if resp["session_id"] != session.ID {
		t.Errorf("unexpected session_id: %v", resp["session_id"])
	}
	if resp["status"] != SessionStatusQueued {
		t.Errorf("unexpected status: %v", resp["status"])
	}
	if resp["current_iteration"] != 1 {
		t.Errorf("unexpected current_iteration: %v", resp["current_iteration"])
	}
	if resp["successful_iterations"] != 1 {
		t.Errorf("unexpected successful_iterations: %v", resp["successful_iterations"])
	}
}

func TestSession_StopRequested(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)
	live, _ := mgr.GetSessionDirect(session.ID)

	if live.StopRequested() {
		t.Error("stop should not be requested initially")
	}

	live.RequestStop()
	if !live.StopRequested() {
		t.Error("stop should be requested after RequestStop")
	}
}

func TestCleanupExpiredSessions(t *testing.T) {
	mgr := newTestManager(t, 1) // 1 second retention

	// Create two sessions and complete them
	s1, _ := mgr.CreateSession("task 1", nil, "", "", 1, 60)
	s2, _ := mgr.CreateSession("task 2", nil, "", "", 1, 60)
	s3, _ := mgr.CreateSession("task 3", nil, "", "", 1, 60) // still running

	mgr.CompleteSession(s1.ID)
	mgr.CompleteSession(s2.ID)

	// Backdate the CompletedAt on s1 so it appears expired
	live1, _ := mgr.GetSessionDirect(s1.ID)
	live1.mu.Lock()
	past := time.Now().Add(-10 * time.Second)
	live1.CompletedAt = &past
	live1.mu.Unlock()

	// Run cleanup
	mgr.cleanupExpiredSessions()

	// s1 should be removed (expired)
	if _, exists := mgr.GetSession(s1.ID); exists {
		t.Error("expected expired session s1 to be removed")
	}

	// s2 should still exist (completed just now, within retention)
	if _, exists := mgr.GetSession(s2.ID); !exists {
		t.Error("expected recently completed session s2 to still exist")
	}

	// s3 should still exist (still running)
	if _, exists := mgr.GetSession(s3.ID); !exists {
		t.Error("expected running session s3 to still exist")
	}
}

func TestStop_Idempotent(t *testing.T) {
	mgr := NewManager(3600, 10)

	// Should not panic when called multiple times
	mgr.Stop()
	mgr.Stop()
	mgr.Stop()
}

func TestContext_CancelledAfterStop(t *testing.T) {
	mgr := NewManager(3600, 10)

	ctx := mgr.Context()
	if ctx.Err() != nil {
		t.Error("expected context to be active before Stop")
	}

	mgr.Stop()

	if ctx.Err() == nil {
		t.Error("expected context to be cancelled after Stop")
	}
}

func TestEnqueue_DispatchesAndRuns(t *testing.T) {
	mgr := newTestManager(t, 3600)

	session, _ := mgr.CreateSession("test", nil, "", "", 1, 60)
	if session.Status != SessionStatusQueued {
		t.Fatalf("expected queued, got %s", session.Status)
	}

	done := make(chan struct{})
	err := mgr.Enqueue(session, func(s *Session) {
		defer close(done)
		// By the time startFunc runs, status should be running
		s.mu.RLock()
		st := s.Status
		s.mu.RUnlock()
		if st != SessionStatusRunning {
			t.Errorf("expected running inside startFunc, got %s", st)
		}
	})
	if err != nil {
		t.Fatalf("unexpected enqueue error: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startFunc was not called within timeout")
	}
}

func TestEnqueue_QueueFull(t *testing.T) {
	mgr := NewManager(3600, 1)
	defer mgr.Stop()

	// Block the dispatch loop by filling the queue with a long-running item
	blocker := make(chan struct{})
	s1, _ := mgr.CreateSession("first", nil, "", "", 1, 60)
	err := mgr.Enqueue(s1, func(s *Session) {
		<-blocker // block until released
	})
	if err != nil {
		t.Fatalf("first enqueue should succeed: %v", err)
	}

	// Wait for dispatch loop to pick up s1 (it will block inside startFunc)
	time.Sleep(100 * time.Millisecond)

	// Now fill the 1-slot buffer
	s2, _ := mgr.CreateSession("second", nil, "", "", 1, 60)
	err = mgr.Enqueue(s2, func(s *Session) {})
	if err != nil {
		t.Fatalf("second enqueue should succeed (fills buffer): %v", err)
	}

	// Third should fail — queue is full
	s3, _ := mgr.CreateSession("third", nil, "", "", 1, 60)
	err = mgr.Enqueue(s3, func(s *Session) {})
	if err == nil {
		t.Error("expected queue full error")
	}

	close(blocker)
}

func TestStop_DrainsQueue(t *testing.T) {
	mgr := NewManager(3600, 10)

	// Block dispatch so items stay queued
	blocker := make(chan struct{})
	s1, _ := mgr.CreateSession("blocking", nil, "", "", 1, 60)
	mgr.Enqueue(s1, func(s *Session) {
		<-blocker
	})
	// Wait for dispatch to pick up s1
	time.Sleep(100 * time.Millisecond)

	// Enqueue more items that will be in the channel buffer
	var sessions []*Session
	for i := 0; i < 3; i++ {
		s, _ := mgr.CreateSession("queued", nil, "", "", 1, 60)
		mgr.Enqueue(s, func(s *Session) {})
		sessions = append(sessions, s)
	}

	// Unblock s1 and stop
	close(blocker)
	time.Sleep(50 * time.Millisecond)
	mgr.Stop()

	// Give drain a moment to process
	time.Sleep(100 * time.Millisecond)

	// Queued sessions that didn't run should be failed
	// (Some may have run between unblock and stop, so just check they're not queued)
	for _, s := range sessions {
		snap, _ := mgr.GetSession(s.ID)
		if snap.Status == SessionStatusQueued {
			t.Errorf("expected session %s to not be queued after drain", s.ID)
		}
	}
}

func TestQueueLength(t *testing.T) {
	mgr := NewManager(3600, 10)
	defer mgr.Stop()

	if mgr.QueueLength() != 0 {
		t.Errorf("expected queue length 0, got %d", mgr.QueueLength())
	}

	// Block dispatch
	blocker := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	s1, _ := mgr.CreateSession("blocking", nil, "", "", 1, 60)
	mgr.Enqueue(s1, func(s *Session) {
		wg.Done()
		<-blocker
	})
	wg.Wait() // wait for dispatch to pick up s1

	// Enqueue two more — they stay in the buffer
	s2, _ := mgr.CreateSession("q1", nil, "", "", 1, 60)
	mgr.Enqueue(s2, func(s *Session) {})
	s3, _ := mgr.CreateSession("q2", nil, "", "", 1, 60)
	mgr.Enqueue(s3, func(s *Session) {})

	if mgr.QueueLength() != 2 {
		t.Errorf("expected queue length 2, got %d", mgr.QueueLength())
	}

	close(blocker)
}
