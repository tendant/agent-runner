package execution

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/repos"
)

func TestRuntimeInstructions(t *testing.T) {
	host := runtimeInstructions(runtimeFacts{MemoryDir: "/data/memory"})
	box := runtimeInstructions(runtimeFacts{Sandboxed: true, MemoryDir: "/state/home/s1/memory", MemoryCopy: true})

	for _, want := range []string{"agent-runner pushes", "/repo add", "/data/memory", "several people", "do not edit them", "_send/"} {
		if !strings.Contains(host, want) {
			t.Errorf("unsandboxed: missing %q:\n%s", want, host)
		}
	}
	if strings.Contains(host, "your own copy") {
		t.Errorf("unsandboxed sessions must not get the sandbox's rules:\n%s", host)
	}
	for _, want := range []string{"do not `git push`", "agent-runner pushes", "/state/home/s1/memory", "your own copy", "never yours to use"} {
		if !strings.Contains(box, want) {
			t.Errorf("sandboxed: missing %q:\n%s", want, box)
		}
	}
	repoNote := runtimeInstructions(runtimeFacts{Repos: []repos.Repo{{Name: "oam-server", URL: "https://lei@gitee.com/o/oam-server.git", Branch: "develop", Push: true}, {Name: "docs", URL: "https://github.com/o/docs.git"}}})
	for _, want := range []string{"- `oam-server/`: https://lei@gitee.com/o/oam-server.git, on develop, pushed for you", "- `docs/`: https://github.com/o/docs.git, on its default branch, read-only"} {
		if !strings.Contains(repoNote, want) {
			t.Errorf("repo list: missing %q:\n%s", want, repoNote)
		}
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
