package execution

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
)

func TestRuntimeInstructions(t *testing.T) {
	host := runtimeInstructions(runtimeFacts{MemoryDir: "/data/memory"})
	box := runtimeInstructions(runtimeFacts{Sandboxed: true, MemoryDir: "/state/home/s1/memory", MemoryCopy: true})

	for _, want := range []string{"git pull --rebase", "push it to origin", "/data/memory", "several people", "do not edit them", "_send/"} {
		if !strings.Contains(host, want) {
			t.Errorf("unsandboxed: missing %q:\n%s", want, host)
		}
	}
	if strings.Contains(host, "your own copy") {
		t.Errorf("unsandboxed sessions must not get the sandbox's rules:\n%s", host)
	}
	for _, want := range []string{"do not `git push`", "the runner pushes", "/state/home/s1/memory", "your own copy", "never yours to use"} {
		if !strings.Contains(box, want) {
			t.Errorf("sandboxed: missing %q:\n%s", want, box)
		}
	}
	if strings.Contains(box, "push it to origin") {
		t.Errorf("a sandboxed agent must not be told to push:\n%s", box)
	}
	if strings.Contains(runtimeInstructions(runtimeFacts{}), "### Memory") {
		t.Error("no memory section without a memory dir")
	}
}

// The prompt names the agent user's secrets, never their values.
func TestSessionPromptNamesSecrets(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	h.config.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	if err := agentsecrets.New(h.config.SecretsFile).Set("GITEE_TOKEN", "tok-secret-value", "Gitee API for oam-server"); err != nil {
		t.Fatal(err)
	}
	var prompt string
	h.executor = promptExecutor{fn: func(p string) {
		if prompt == "" {
			prompt = p
		}
	}}
	session, _ := h.agentManager.CreateSession("check", nil, "test", "", 1, 30)
	h.ExecuteAgent(session)
	if !strings.Contains(prompt, "`$GITEE_TOKEN`: Gitee API for oam-server") {
		t.Errorf("secret not named in the prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "tok-secret-value") {
		t.Error("a secret's value must never be in the prompt")
	}
}
