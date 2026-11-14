package execution

import (
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
)

const secretPrompt = "# Agent instructions: the whole system prompt, data access rules, memory..."

// A failed prompt's raw CLI output (pi's RPC stream, which echoes the
// prompt) is diagnostics, not the model's reply.
func TestRecordFailedOutputKeepsRawOutOfOutput(t *testing.T) {
	var r agent.IterationResult
	recordFailedOutput(&r, &executor.ExecutionResult{RawOutput: `{"type":"message_start","message":{"role":"user","content":"` + secretPrompt + `"}}`, CostUSD: 0.01})
	if r.Output != "" {
		t.Errorf("Output = %q, want empty: raw output is not the model's reply", r.Output)
	}
	if !strings.Contains(r.Diagnostics, secretPrompt) || r.CostUSD != 0.01 {
		t.Errorf("diagnostics %q cost %v", r.Diagnostics, r.CostUSD)
	}
	r = agent.IterationResult{}
	recordFailedOutput(&r, &executor.ExecutionResult{Output: "partial answer", RawOutput: "raw"})
	if r.Output != "partial answer" || r.Diagnostics != "raw" {
		t.Errorf("with model text: %+v", r)
	}
	recordFailedOutput(&r, nil) // no result: nothing to record, no panic
}

// The chat report of a failed session carries no prompt, and enough of the
// error to say what failed.
func TestFailedSessionReportHasNoPrompt(t *testing.T) {
	errText := "the LLM provider account is out of quota or credit — check usage/billing on the provider's dashboard\n\n" +
		`Details: agent execution failed: pi: model call failed: 429: {"message":"Budget exceeded for API key","type":"insufficient_quota"}`
	var it agent.IterationResult
	it.Status, it.Error = agent.IterationStatusError, errText
	recordFailedOutput(&it, &executor.ExecutionResult{RawOutput: secretPrompt})
	snap := &agent.Session{ID: "s1", Source: "api", Status: agent.SessionStatusFailed, Message: "check the watchlist",
		Error: errText, Iterations: []agent.IterationResult{it}}

	n := &recNotifier{}
	(&Engine{config: &config.Config{}, deps: notifyDeps{n: n}}).notifySessionResult(snap)
	if strings.Contains(n.text, secretPrompt) {
		t.Fatalf("the chat report posted the prompt:\n%s", n.text)
	}
	if !strings.HasPrefix(n.text, "Agent failed") || !strings.Contains(n.text, "Budget exceeded for API key") {
		t.Errorf("report should say the session failed and why:\n%s", n.text)
	}
}
