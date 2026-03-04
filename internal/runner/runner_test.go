package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	simpleworkflow "github.com/tendant/simple-workflow"
)

// mockExecutor records calls to ExecuteAgentTask.
type mockExecutor struct {
	calls   []AgentTaskPayload
	execErr error
}

func (m *mockExecutor) ExecuteAgentTask(_ context.Context, payload AgentTaskPayload) error {
	m.calls = append(m.calls, payload)
	return m.execErr
}

func TestHybridRunnerSQLite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exec := &mockExecutor{}

	r, err := New(Config{
		DatabaseURL:       "sqlite://:memory:",
		AgentID:           "test-runner",
		LeaseDuration:     30 * time.Second,
		PollCap:           1 * time.Second,
		HeartbeatInterval: 5 * time.Minute,
		MaxAttempts:       3,
		TypePrefix:        "agent.",
	}, exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Stop()

	// Run migrations
	swClient := simpleworkflow.NewClientWithDB(r.db, r.dialect)
	if err := swClient.AutoMigrate(ctx); err != nil {
		t.Fatalf("simple-workflow migrate: %v", err)
	}
	if err := AutoMigrate(ctx, r.db, r.driverName); err != nil {
		t.Fatalf("runner migrate: %v", err)
	}

	// Submit a task
	payload := AgentTaskPayload{
		Message: "test task",
		Paths:   []string{"/tmp"},
	}
	payloadJSON, _ := json.Marshal(payload)
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO workflow_run (id, type, payload, status, priority, run_at, attempt, max_attempts, created_at, updated_at)
		VALUES (?, ?, ?, 'pending', 100, datetime('now'), 0, 3, datetime('now'), datetime('now'))
	`, "test-1", "agent.run", string(payloadJSON))
	if err != nil {
		t.Fatalf("insert workflow_run: %v", err)
	}

	// Claim and execute
	claimed := r.claimAndExecuteOne(ctx)
	if !claimed {
		t.Fatal("expected to claim a task")
	}

	if len(exec.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(exec.calls))
	}
	if exec.calls[0].Message != "test task" {
		t.Errorf("expected message 'test task', got %q", exec.calls[0].Message)
	}

	// Verify no more work
	if r.claimAndExecuteOne(ctx) {
		t.Error("expected no more work")
	}
}

func TestHeartbeatStore(t *testing.T) {
	ctx := context.Background()

	dialect, dsn, _ := simpleworkflow.DetectDialect("sqlite://:memory:")
	db, err := dialect.OpenDB(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := AutoMigrate(ctx, db, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := NewHeartbeatStore(db, "sqlite")

	// Upsert
	if err := store.Upsert(ctx, "agent-1", "running", "task-1", "", "step 1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Get
	hb, err := store.Get(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if hb == nil {
		t.Fatal("expected heartbeat")
	}
	if hb.Status != "running" {
		t.Errorf("expected status 'running', got %q", hb.Status)
	}

	// Touch
	if err := store.Touch(ctx, "agent-1"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	// Get non-existent
	hb, err = store.Get(ctx, "agent-2")
	if err != nil {
		t.Fatalf("get non-existent: %v", err)
	}
	if hb != nil {
		t.Error("expected nil for non-existent agent")
	}
}

func TestListenerNilOnSQLite(t *testing.T) {
	l := NewListener("sqlite", "")
	if l != nil {
		t.Error("expected nil listener for sqlite")
	}

	// Wake() on nil listener should return a channel that never fires
	var nilL *Listener
	ch := nilL.Wake()
	select {
	case <-ch:
		t.Error("nil listener Wake() should never fire")
	case <-time.After(10 * time.Millisecond):
		// expected
	}

	// Close on nil should not panic
	nilL.Close()
}
