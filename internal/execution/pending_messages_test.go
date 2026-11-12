package execution

import (
	"strings"
	"testing"
)

// A message queued for a running session (POST /agent/messages, one-shot
// backend) reaches every later iteration, not only the next one: a one-shot
// CLI gets the full prompt each time and keeps nothing between them.
func TestPendingMessageKeptForLaterIterations(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.Engine.config.Agent.PlannerEnabled = false
	session, err := env.handlers.agentManager.CreateSession("build the page", nil, "test", "", 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	env.handlers.executor = promptExecutor{fn: func(p string) {
		prompts = append(prompts, p)
		if len(prompts) == 1 {
			session.AppendPendingMessage("use the blue theme")
		}
	}}
	env.handlers.ExecuteAgent(session)

	if len(prompts) != 3 {
		t.Fatalf("got %d prompts, want 3 (status %s)", len(prompts), session.Snapshot().Status)
	}
	if strings.Contains(prompts[0], "blue theme") {
		t.Error("the message reached the iteration before it was sent")
	}
	for i, p := range prompts[1:] {
		if !strings.Contains(p, "use the blue theme") {
			t.Errorf("iteration %d lost the queued message", i+2)
		}
	}
}
