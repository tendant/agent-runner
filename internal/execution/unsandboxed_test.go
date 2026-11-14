package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
)

// With the sandbox off, a session still lands on the audit record, as an
// unsandboxed run, and runs as before.
func TestUnsandboxedSessionIsRecorded(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.Engine.config.Agent.PlannerEnabled = false
	env.handlers.backend = &scriptedBackend{factory: func(int) *scriptedSession {
		return newScriptedSession([]string{"TASK: DONE"}, 0, 0)
	}}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	rec, err := audit.OpenFileRecorder(path, "test-host", audit.FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rt, err := sandboxrt.New(sandboxrt.Config{Mode: sandbox.ModeOff, Audit: rec, Agent: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	env.handlers.Engine.SetSandbox(rt)

	s, _ := env.handlers.agentManager.CreateSession("do it", nil, "alice", "", 3, 60)
	env.handlers.ExecuteAgentWithContext(context.Background(), s)
	if st := s.Snapshot().Status; st != agent.SessionStatusCompleted {
		t.Fatalf("status %s (%s)", st, s.Snapshot().Error)
	}
	f, _ := os.Open(path)
	defer f.Close()
	var types []string
	d := audit.NewDecoder(f, 0)
	for {
		e, err := d.Next()
		if err != nil {
			break
		}
		if e.RunID != s.ID {
			t.Errorf("event for run %q", e.RunID)
		}
		types = append(types, e.Type)
		if e.Type == audit.TypeSandboxCreated {
			var dl audit.LifecycleDetail
			json.Unmarshal(e.Detail, &dl)
			if dl.Backend != sandboxrt.BackendNone || dl.Agent != "stock" {
				t.Errorf("created %s", e.Detail)
			}
		}
	}
	if len(types) != 3 || types[0] != audit.TypePolicySnapshot || types[2] != audit.TypeSandboxClosed {
		t.Errorf("events %v", types)
	}
}
