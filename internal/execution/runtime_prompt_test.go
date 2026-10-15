package execution

import (
	"context"
	"github.com/agent-runner/agent-runner/internal/executor"
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

// A secret value the agent echoes is redacted in what the session records
// (and so in chat, the API and the audit log).
func TestSessionRedactsSecretValues(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	h.config.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	agentsecrets.New(h.config.SecretsFile).Set("GITEE_TOKEN", "gitee-secret-value-1", "")
	h.executor = echoExecutor{text: "the token is gitee-secret-value-1"}
	session, _ := h.agentManager.CreateSession("show it", nil, "test", "", 1, 30)
	h.ExecuteAgent(session)
	for _, it := range session.Snapshot().Iterations {
		if strings.Contains(it.Output, "gitee-secret-value-1") || !strings.Contains(it.Output, "[redacted:GITEE_TOKEN]") {
			t.Errorf("output not redacted: %q", it.Output)
		}
	}
}

type echoExecutor struct{ text string }

func (e echoExecutor) Execute(context.Context, string, string) (*executor.ExecutionResult, error) {
	return &executor.ExecutionResult{Output: e.text}, nil
}
func (e echoExecutor) ExecuteWithSystemPrompt(ctx context.Context, w, _, in string) (*executor.ExecutionResult, error) {
	return e.Execute(ctx, w, in)
}
func (e echoExecutor) ExecuteWithLog(ctx context.Context, w, in string) (*executor.ExecutionResult, string, error) {
	r, err := e.Execute(ctx, w, in)
	return r, "", err
}
func (e echoExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, w, _, in string) (*executor.ExecutionResult, string, error) {
	r, err := e.Execute(ctx, w, in)
	return r, "", err
}
