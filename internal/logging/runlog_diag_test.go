package logging

import (
	"strings"
	"testing"
)

// A failed iteration's raw CLI output stays in the session log, under its
// own label, apart from the model's output.
func TestRunLogShowsDiagnosticsApart(t *testing.T) {
	out := (&RunLogger{}).generateAgentMarkdown(&AgentLogData{SessionID: "s1", Status: "failed",
		Iterations: []AgentIterationLog{{Iteration: 1, Status: "error", Error: "model call failed", Diagnostics: "raw pi stream"}}}, "t")
	if !strings.Contains(out, "CLI output (diagnostics") || !strings.Contains(out, "raw pi stream") {
		t.Errorf("log:\n%s", out)
	}
	if strings.Contains(out, "<summary>Output (") {
		t.Errorf("diagnostics shown as the model's output:\n%s", out)
	}
}
