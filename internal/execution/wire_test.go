package execution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/subagent"
)

// A run's result crosses to the dispatcher redacted, with its plan and
// review typed again, and finalizes there like a local one: the audit log
// is written and holds no secret.
func TestRunResultWireRoundTrip(t *testing.T) {
	env := setupTestEnv(t)
	s, err := env.handlers.agentManager.CreateSession("deploy with s3cr3t", nil, "alice", "", 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPlanResult(&subagent.PlanResult{Summary: "deploy", Steps: []subagent.PlanStep{{ID: "1", Description: "go"}}}, 1)
	s.SetReviewResult(&subagent.ReviewResult{Complete: true, Score: 8})
	s.AddIteration(agent.IterationResult{Iteration: 1, Status: agent.IterationStatusSuccess, Prompt: "token s3cr3t", Output: "done"})
	env.handlers.FailSession(s.ID, "push failed for s3cr3t")
	redact := func(x string) string { return strings.ReplaceAll(x, "s3cr3t", "[redacted]") }

	res := &RunResult{SessionID: s.ID, Source: "api", Message: s.Message, Author: "alice",
		StartTime: time.Now(), Snapshot: s.Snapshot(), PlannerPrompt: "plan with s3cr3t", Redact: redact}
	w, err := res.Wire()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	// What finalization logs is redacted. (The snapshot's message is the
	// request the dispatcher itself handed the runner, and is not logged.)
	for name, v := range map[string]string{"message": w.Message, "planner prompt": w.PlannerPrompt,
		"iteration prompt": w.Snapshot.Iterations[0].Prompt, "error": w.Snapshot.Error} {
		if strings.Contains(v, "s3cr3t") || !strings.Contains(v, "[redacted]") {
			t.Errorf("%s not redacted: %q", name, v)
		}
	}
	var got RunResultWire
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	back, err := got.RunResult()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := back.Snapshot.PlanJSON.(*subagent.PlanResult); !ok || p.Summary != "deploy" {
		t.Errorf("plan = %#v", back.Snapshot.PlanJSON)
	}
	if r, ok := back.Snapshot.ReviewJSON.(*subagent.ReviewResult); !ok || r.Score != 8 {
		t.Errorf("review = %#v", back.Snapshot.ReviewJSON)
	}
	if back.Snapshot.Status != agent.SessionStatusFailed || back.Author != "alice" {
		t.Errorf("status %s author %q", back.Snapshot.Status, back.Author)
	}

	env.handlers.finalizeSession(back)
	logs, _ := filepath.Glob(filepath.Join(env.handlers.Engine.config.LogsRoot, "*.md"))
	if len(logs) != 1 {
		t.Fatalf("%d audit logs", len(logs))
	}
	b, _ := os.ReadFile(logs[0])
	if strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), "[redacted]") {
		t.Errorf("audit log:\n%s", b)
	}
}

// A result whose snapshot is another session's is refused.
func TestRunResultWireChecksSession(t *testing.T) {
	w := &RunResultWire{SessionID: "agent-1", Snapshot: &agent.SessionWire{ID: "agent-2"}}
	if _, err := w.RunResult(); err == nil {
		t.Error("accepted another session's snapshot")
	}
	if _, err := (&RunResultWire{SessionID: "agent-1"}).RunResult(); err == nil {
		t.Error("accepted a result without a snapshot")
	}
}
