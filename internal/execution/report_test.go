package execution

import (
	"context"
	"os"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// captureReporter keeps the result instead of finalizing, as a job's
// reporter will, and records whether the workspace still existed then.
type captureReporter struct {
	live         *agent.Session
	res          *RunResult
	workspaceWas bool
}

func (c *captureReporter) Finish(res *RunResult) {
	c.res = res
	if ws := c.live.WorkspacePath; ws != "" {
		_, err := os.Stat(ws)
		c.workspaceWas = err == nil
	}
}

// The run hands finalization everything it needs and does none of it
// itself: with a reporter that only keeps the result, no audit log is
// written, and the workspace is cleaned up only after the reporter is done.
func TestRunSessionReportsInsteadOfFinalizing(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.Engine.config.Agent.PlannerEnabled = false
	env.handlers.backend = &scriptedBackend{factory: func(int) *scriptedSession {
		return newScriptedSession([]string{"TASK: DONE"}, 0, 0)
	}}
	session, err := env.handlers.agentManager.CreateSession("do something", nil, "alice", "", 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	session.Source = "test"

	rep := &captureReporter{live: session}
	env.handlers.runSession(context.Background(), session, rep)

	res := rep.res
	if res == nil {
		t.Fatal("reporter got no result")
	}
	if res.SessionID != session.ID || res.Source != "test" || res.Message != "do something" || res.Author != "alice" || res.Eval {
		t.Errorf("result = %+v", res)
	}
	if res.Snapshot == nil || res.Snapshot.Status != agent.SessionStatusCompleted {
		t.Fatalf("snapshot status = %v", res.Snapshot)
	}
	if len(res.PromptSections) == 0 || res.Redact == nil || res.StartTime.IsZero() {
		t.Errorf("prompt outline %d sections, redact set %v, start %v", len(res.PromptSections), res.Redact != nil, res.StartTime)
	}
	if !rep.workspaceWas {
		t.Error("workspace was gone before the reporter finished")
	}
	if session.WorkspacePath == "" {
		t.Fatal("the run had no workspace")
	}
	if _, err := os.Stat(session.WorkspacePath); err == nil {
		t.Error("workspace not cleaned up after the reporter finished")
	}
	if ents, _ := os.ReadDir(env.handlers.Engine.config.LogsRoot); len(ents) != 0 {
		t.Errorf("the run wrote %d audit log entries itself", len(ents))
	}

	// The local reporter finalizes: the same result gives an audit log.
	env.handlers.finalizeSession(res)
	if ents, _ := os.ReadDir(env.handlers.Engine.config.LogsRoot); len(ents) == 0 {
		t.Error("finalizeSession wrote no audit log")
	}
}
