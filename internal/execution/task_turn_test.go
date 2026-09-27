package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// resumingExecutor is a scriptedExecutor that can also resume: it records
// the conversation ref and whether each prompt carried the full prompt.
type resumingExecutor struct {
	scriptedExecutor
	refs  []string
	fulls []bool
	n     int
}

func (r *resumingExecutor) ResumeKind() string { return "fake" }
func (r *resumingExecutor) ExecuteResuming(_ context.Context, w, _, msg, ref string, _ func(executor.EventKind, string)) (*executor.ExecutionResult, string, error) {
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	r.fulls = append(r.fulls, msg == "deploy" || msg == "turn two")
	if ref == "" {
		r.n++
		ref = fmt.Sprintf("conv-%d", r.n)
	}
	r.mu.Unlock()
	res, err := r.run(w)
	return res, ref, err
}

// A task's turns continue one backend conversation: iterations after the
// first send incremental prompts, and the next turn resumes the same ref.
func TestTaskTurns_ResumeBackendConversation(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.config.Agent.PlannerEnabled = false
	env.handlers.config.Agent.ReviewerEnabled = false
	env.handlers.config.Agent.TaskResumeBackend = true
	taskDir := env.handlers.TaskWorkspacePath("m_1")

	re := &resumingExecutor{}
	re.fn = func(checkout string, call int) {
		switch call {
		case 2: // second iteration of turn 1 asks the user
			writeJSON(t, filepath.Join(checkout, "_progress.json"), map[string]any{"status": "needs_input", "question": "Which env?"})
		case 3:
			writeJSON(t, filepath.Join(checkout, "_progress.json"), map[string]any{"status": "done"})
		}
	}
	env.handlers.executor = re

	for _, msg := range []string{"deploy", "turn two"} {
		s, err := env.handlers.agentManager.CreateSession(msg, nil, "test", "m_1", 5, 60)
		if err != nil {
			t.Fatal(err)
		}
		s.TaskWorkspace = taskDir
		env.handlers.ExecuteAgent(s)
		if snap := s.Snapshot(); snap.Status != "completed" {
			t.Fatalf("turn %q: %s (%s)", msg, snap.Status, snap.Error)
		}
	}

	if got := strings.Join(re.refs, ","); got != ",conv-1,conv-1" {
		t.Fatalf("refs = %q, want a new conversation, then the same one twice", got)
	}
	if want := []bool{true, false, true}; fmt.Sprint(re.fulls) != fmt.Sprint(want) {
		t.Fatalf("full prompts = %v, want %v (incremental within a turn, full at a turn's start)", re.fulls, want)
	}
}

// With AGENT_TASK_RESUME_BACKEND off, turns never resume a conversation.
func TestTaskTurns_ResumeBackendDisabled(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.config.Agent.PlannerEnabled = false
	env.handlers.config.Agent.TaskResumeBackend = false
	re := &resumingExecutor{}
	re.fn = func(checkout string, _ int) {
		writeJSON(t, filepath.Join(checkout, "_progress.json"), map[string]any{"status": "done"})
	}
	env.handlers.executor = re
	s, _ := env.handlers.agentManager.CreateSession("deploy", nil, "test", "m_1", 5, 60)
	s.TaskWorkspace = env.handlers.TaskWorkspacePath("m_1")
	env.handlers.ExecuteAgent(s)
	if len(re.refs) != 0 {
		t.Fatalf("resumed with the backend resume disabled: %v", re.refs)
	}
}

// cannedLLM returns a fixed completion and records prompts.
type cannedLLM struct {
	out     string
	prompts []string
}

func (c *cannedLLM) Complete(_ context.Context, prompt string) (string, error) {
	c.prompts = append(c.prompts, prompt)
	return c.out, nil
}

func revisedPlanJSON(steps ...string) string {
	return `{"summary":"s","approach":"a","steps":[` + strings.Join(steps, ",") + `]}`
}

func seedPlan(t *testing.T, taskDir string) {
	t.Helper()
	os.MkdirAll(taskDir, 0o755)
	if err := subagent.SavePlan(taskDir, &subagent.PlanResult{Summary: "s", Steps: []subagent.PlanStep{
		{ID: "1", Description: "build", Done: true}, {ID: "2", Description: "deploy"},
	}}); err != nil {
		t.Fatal(err)
	}
}

// Feedback on a task revises its saved plan. A small revision is saved and
// work continues on it; a large one ends the turn before any work to ask
// for approval.
func TestTaskTurns_PlanRevision(t *testing.T) {
	for _, tc := range []struct {
		name         string
		revised      string
		wantFirst    string // saved plan's first two step descriptions
		wantApproval bool
	}{
		{"small", revisedPlanJSON(`{"id":"1","description":"build","done":true}`, `{"id":"2","description":"deploy to staging"}`), "build/deploy to staging", false},
		{"reopens finished work", revisedPlanJSON(`{"id":"1","description":"build again","done":false}`, `{"id":"2","description":"deploy"}`), "build again/deploy", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupTestEnv(t)
			env.handlers.config.Agent.PlannerEnabled = true
			env.handlers.config.Agent.ReviewerEnabled = false
			planner := &cannedLLM{out: tc.revised}
			env.handlers.planner = planner
			exec := &scriptedExecutor{fn: func(checkout string, _ int) {
				writeJSON(t, filepath.Join(checkout, "_progress.json"), map[string]any{"status": "done"})
			}}
			env.handlers.executor = exec
			taskDir := env.handlers.TaskWorkspacePath("m_1")
			seedPlan(t, taskDir)

			s, err := env.handlers.agentManager.CreateSession("## Task ... feedback", nil, "test", "m_1", 5, 60)
			if err != nil {
				t.Fatal(err)
			}
			s.TaskWorkspace = taskDir
			s.TaskFeedback = "deploy to staging, not prod"
			env.handlers.ExecuteAgent(s)

			snap := s.Snapshot()
			if snap.Status != "completed" {
				t.Fatalf("status %s (%s)", snap.Status, snap.Error)
			}
			if len(planner.prompts) != 1 || !strings.Contains(planner.prompts[0], "User feedback: deploy to staging, not prod") || !strings.Contains(planner.prompts[0], `"description": "build"`) {
				t.Fatalf("revise prompt missing plan or feedback: %v", planner.prompts)
			}
			saved := subagent.LoadPlan(taskDir)
			if saved == nil || len(saved.Steps) != 2 || saved.Steps[0].Description+"/"+saved.Steps[1].Description != tc.wantFirst {
				t.Fatalf("saved plan = %+v, want %s", saved, tc.wantFirst)
			}
			if snap.TurnApproval != tc.wantApproval {
				t.Fatalf("approval = %v, want %v", snap.TurnApproval, tc.wantApproval)
			}
			if tc.wantApproval {
				if exec.calls != 0 || !strings.Contains(snap.TurnQuestion, "Proceed with this plan?") || !strings.Contains(snap.TurnQuestion, "⬜ 1. build again") {
					t.Fatalf("approval turn: %d agent calls, question %q", exec.calls, snap.TurnQuestion)
				}
			} else if exec.calls == 0 {
				t.Fatal("a small revision should go straight to work")
			}
		})
	}
}
