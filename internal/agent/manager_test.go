package agent

import (
	"testing"
)

func TestCreateSession_Success(t *testing.T) {
	mgr := NewManager()

	session, err := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.ID == "" {
		t.Error("expected non-empty session ID")
	}
	if session.Status != SessionStatusRunning {
		t.Errorf("expected status running, got %s", session.Status)
	}
	if session.MaxIterations != 10 {
		t.Errorf("expected max_iterations 10, got %d", session.MaxIterations)
	}
}

func TestGetSession_Exists(t *testing.T) {
	mgr := NewManager()

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
	mgr := NewManager()

	_, exists := mgr.GetSession("nonexistent")
	if exists {
		t.Error("expected session to not exist")
	}
}

func TestStopSession(t *testing.T) {
	mgr := NewManager()

	session, _ := mgr.CreateSession("fix the bug", []string{"src/"}, "bot", "[agent]", 10, 300)

	if err := mgr.StopSession(session.ID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snap, _ := mgr.GetSession(session.ID)
	if snap.Status != SessionStatusStopping {
		t.Errorf("expected status stopping, got %s", snap.Status)
	}
}

func TestStopSession_NotFound(t *testing.T) {
	mgr := NewManager()

	if err := mgr.StopSession("nonexistent"); err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestCompleteSession(t *testing.T) {
	mgr := NewManager()

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
	mgr := NewManager()

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
	mgr := NewManager()

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
	if snap.TotalCommits != 1 {
		t.Errorf("expected 1 commit, got %d", snap.TotalCommits)
	}
	if len(snap.Iterations) != 1 {
		t.Fatalf("expected 1 iteration, got %d", len(snap.Iterations))
	}
	if snap.Iterations[0].Commit != "abc123" {
		t.Errorf("expected commit abc123, got %s", snap.Iterations[0].Commit)
	}
}

func TestSession_ConsecutiveFailures(t *testing.T) {
	mgr := NewManager()

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
	mgr := NewManager()

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
	if resp["status"] != SessionStatusRunning {
		t.Errorf("unexpected status: %v", resp["status"])
	}
	if resp["current_iteration"] != 1 {
		t.Errorf("unexpected current_iteration: %v", resp["current_iteration"])
	}
	if resp["total_commits"] != 1 {
		t.Errorf("unexpected total_commits: %v", resp["total_commits"])
	}
}

func TestSession_StopRequested(t *testing.T) {
	mgr := NewManager()

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
