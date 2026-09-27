package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/subagent"
)

// scriptedExecutor runs fn for every agent invocation, in the checkout dir.
type scriptedExecutor struct {
	mu    sync.Mutex
	calls int
	fn    func(checkout string, call int)
}

func (s *scriptedExecutor) run(w string) (*executor.ExecutionResult, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	s.fn(w, n)
	return &executor.ExecutionResult{Output: "ok"}, nil
}
func (s *scriptedExecutor) Execute(_ context.Context, w, _ string) (*executor.ExecutionResult, error) {
	return s.run(w)
}
func (s *scriptedExecutor) ExecuteWithSystemPrompt(_ context.Context, w, _, _ string) (*executor.ExecutionResult, error) {
	return s.run(w)
}
func (s *scriptedExecutor) ExecuteWithLog(_ context.Context, w, _ string) (*executor.ExecutionResult, string, error) {
	r, err := s.run(w)
	return r, "", err
}
func (s *scriptedExecutor) ExecuteWithLogAndSystemPrompt(_ context.Context, w, _, _ string) (*executor.ExecutionResult, string, error) {
	r, err := s.run(w)
	return r, "", err
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two turns of one task: the first stops on needs_input after one iteration
// and keeps its workspace; the second continues in it with the turn fields
// reset and earlier outputs not re-sent; finishing releases the workspace.
func TestTaskTurns_NeedsInputThenContinue(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.config.Agent.PlannerEnabled = false
	env.handlers.config.Agent.ReviewerEnabled = false
	taskDir := env.handlers.TaskWorkspacePath("m_1")

	var sawScratch, sawStaleStatus, sawStaleSend bool
	exec := &scriptedExecutor{fn: func(checkout string, call int) {
		switch call {
		case 1:
			os.WriteFile(filepath.Join(checkout, "notes.txt"), []byte("scratch"), 0o644)
			os.MkdirAll(filepath.Join(checkout, "_send"), 0o755)
			os.WriteFile(filepath.Join(checkout, "_send", "report.txt"), []byte("r"), 0o644)
			writeJSON(t, filepath.Join(checkout, "_progress.json"), map[string]any{
				"completed_steps": []string{"1"},
				"status":          "needs_input",
				"question":        "Which environment?",
				"summary":         "looked around",
				"decisions":       []string{"use staging db"},
			})
		default:
			_, err := os.Stat(filepath.Join(checkout, "notes.txt"))
			sawScratch = err == nil
			p := subagent.ReadProgress(checkout)
			sawStaleStatus = p.Status != "" || p.Question != ""
			_, err = os.Stat(filepath.Join(checkout, "_send", "report.txt"))
			sawStaleSend = err == nil
			writeJSON(t, filepath.Join(checkout, "_progress.json"), map[string]any{
				"completed_steps": []string{"1", "2"},
				"status":          "done",
				"summary":         "deployed to staging",
			})
		}
	}}
	env.handlers.executor = exec

	s1, err := env.handlers.agentManager.CreateSession("deploy", nil, "test", "m_1", 5, 60)
	if err != nil {
		t.Fatal(err)
	}
	s1.TaskWorkspace = taskDir
	env.handlers.ExecuteAgent(s1)

	snap := s1.Snapshot()
	if snap.Status != "completed" {
		t.Fatalf("turn 1 status = %s (%s), want completed", snap.Status, snap.Error)
	}
	if len(snap.Iterations) != 1 {
		t.Fatalf("turn 1 ran %d iterations, want 1 (stop on needs_input)", len(snap.Iterations))
	}
	if snap.TurnStatus != subagent.TurnNeedsInput || snap.TurnQuestion != "Which environment?" || snap.TurnSummary != "looked around" {
		t.Fatalf("turn result = %q %q %q", snap.TurnStatus, snap.TurnQuestion, snap.TurnSummary)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "workspace", "notes.txt")); err != nil {
		t.Fatalf("task workspace not kept after turn 1: %v", err)
	}

	s2, err := env.handlers.agentManager.CreateSession("## Task ... User's reply: staging", nil, "test", "m_1", 5, 60)
	if err != nil {
		t.Fatal(err)
	}
	s2.TaskWorkspace = taskDir
	env.handlers.ExecuteAgent(s2)

	snap = s2.Snapshot()
	if snap.Status != "completed" || snap.TurnStatus != subagent.TurnDone || len(snap.Iterations) != 1 {
		t.Fatalf("turn 2: status %s, turn %q, %d iterations", snap.Status, snap.TurnStatus, len(snap.Iterations))
	}
	if !sawScratch {
		t.Error("turn 2 did not see turn 1's scratch file")
	}
	if sawStaleStatus {
		t.Error("turn 2 started with turn 1's status/question still in _progress.json")
	}
	if sawStaleSend {
		t.Error("turn 2 started with turn 1's _send/ output (would be re-sent)")
	}
	if p := subagent.ReadProgress(filepath.Join(taskDir, "workspace")); len(p.CompletedSteps) != 2 {
		t.Errorf("completed_steps = %v, want kept across turns", p.CompletedSteps)
	}

	env.handlers.FinishTaskWorkspace(taskDir)
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("task workspace still present after FinishTaskWorkspace: %v", err)
	}
}

// A one-shot session (no task workspace) still cleans up after itself.
func TestTaskTurns_OneShotStillCleansUp(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.config.Agent.PlannerEnabled = false
	var ws string
	env.handlers.executor = &scriptedExecutor{fn: func(checkout string, _ int) { ws = checkout }}
	s, err := env.handlers.agentManager.CreateSession("hi", nil, "test", "", 1, 60)
	if err != nil {
		t.Fatal(err)
	}
	env.handlers.ExecuteAgent(s)
	if ws == "" {
		t.Fatal("executor never ran")
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("one-shot workspace kept: %v", err)
	}
}
