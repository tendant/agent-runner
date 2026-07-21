package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/llm"
)

// promptDeps is a minimal Deps for prompt-resolution tests — only
// BootstrapPaths is exercised.
type promptDeps struct{ cfg *config.Config }

func (d promptDeps) Executor() executor.Executor       { return nil }
func (d promptDeps) PlannerClient() llm.Client         { return nil }
func (d promptDeps) CuratorClient() llm.Client         { return nil }
func (d promptDeps) Notifier() Notifier                { return nil }
func (d promptDeps) WorkflowClient() WorkflowScheduler { return nil }
func (d promptDeps) BootstrapPaths() (string, string) {
	systemPrompt := d.cfg.Agent.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = filepath.Join(d.cfg.MemoryDir, "agent.md")
	}
	promptFile := d.cfg.Agent.PromptFile
	if promptFile == "" {
		promptFile = filepath.Join(d.cfg.MemoryDir, "prompt.md")
	}
	return systemPrompt, promptFile
}

// makeHandlers builds a minimal Engine wired to a temp dir for prompt tests.
func makeHandlers(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.MemoryDir = filepath.Join(dir, "memory")
	cfg.Agent.SystemPrompt = ""
	cfg.Agent.PromptFile = ""
	os.MkdirAll(cfg.MemoryDir, 0755)
	return &Engine{config: cfg, deps: promptDeps{cfg: cfg}}, dir
}

// TestResolvePrompt_NoFilesReturnsCurrentRequest verifies that when no agent.md,
// prompt.md, or memory files exist, the prompt still contains the current request.
func TestResolvePrompt_NoFilesReturnsCurrentRequest(t *testing.T) {
	h, _ := makeHandlers(t)

	got, err := h.resolvePrompt("do something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The prompt should contain the current request.
	if !strings.Contains(got, "do something") {
		t.Errorf("expected current request in prompt, got %q", got[:min(200, len(got))])
	}
}

// TestResolvePrompt_AgentMdLoadedWithoutEnvVar verifies that memory/agent.md is
// included in the prompt even when AGENT_SYSTEM_PROMPT is not set.
func TestResolvePrompt_AgentMdLoadedWithoutEnvVar(t *testing.T) {
	h, dir := makeHandlers(t)

	agentContent := "CUSTOM AGENT INSTRUCTIONS FOR TEST"
	if err := os.WriteFile(filepath.Join(dir, "memory", "agent.md"), []byte(agentContent), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := h.resolvePrompt("task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, agentContent) {
		t.Errorf("expected agent.md content in prompt; got %q", got[:min(300, len(got))])
	}
}

// TestResolvePrompt_PromptMdLoadedWithoutEnvVar verifies that memory/prompt.md is
// included in the prompt even when AGENT_PROMPT_FILE is not set.
func TestResolvePrompt_PromptMdLoadedWithoutEnvVar(t *testing.T) {
	h, dir := makeHandlers(t)

	promptContent := "CUSTOM WORKFLOW PROMPT FOR TEST"
	if err := os.WriteFile(filepath.Join(dir, "memory", "prompt.md"), []byte(promptContent), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := h.resolvePrompt("task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, promptContent) {
		t.Errorf("expected prompt.md content in prompt; got %q", got[:min(300, len(got))])
	}
}

// TestResolvePrompt_BothFilesLoaded verifies that both agent.md and prompt.md
// are loaded together when neither env var is set.
func TestResolvePrompt_BothFilesLoaded(t *testing.T) {
	h, dir := makeHandlers(t)

	os.WriteFile(filepath.Join(dir, "memory", "agent.md"), []byte("AGENT_INSTRUCTIONS"), 0644)
	os.WriteFile(filepath.Join(dir, "memory", "prompt.md"), []byte("WORKFLOW_STEPS"), 0644)

	got, err := h.resolvePrompt("task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "AGENT_INSTRUCTIONS") {
		t.Error("expected agent.md content")
	}
	if !strings.Contains(got, "WORKFLOW_STEPS") {
		t.Error("expected prompt.md content")
	}
}

// TestResolvePrompt_ExplicitEnvVarOverridesFallback verifies that when
// AGENT_SYSTEM_PROMPT is set, it overrides the ./agent.md fallback.
func TestResolvePrompt_ExplicitEnvVarOverridesFallback(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.MemoryDir = filepath.Join(dir, "memory")

	// Write ./agent.md that should NOT be used
	os.WriteFile(filepath.Join(dir, "agent.md"), []byte("FALLBACK_CONTENT"), 0644)

	// Write explicit file that SHOULD be used
	explicitPath := filepath.Join(dir, "custom-agent.md")
	os.WriteFile(explicitPath, []byte("EXPLICIT_CONTENT"), 0644)
	cfg.Agent.SystemPrompt = explicitPath

	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(dir)

	h := &Engine{config: cfg, deps: promptDeps{cfg: cfg}}
	got, err := h.resolvePrompt("task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "EXPLICIT_CONTENT") {
		t.Error("expected explicit prompt content")
	}
	// The fallback ./agent.md should not appear (bootstrapPaths returns the
	// explicit path, not the default, when SystemPrompt is set).
	if strings.Contains(got, "FALLBACK_CONTENT") {
		t.Error("fallback agent.md should not appear when explicit path is set")
	}
}

// TestResolvePrompt_MissingFallbackFilesNoError verifies that missing ./agent.md
// and ./prompt.md do not cause an error — just silently skip.
func TestResolvePrompt_MissingFallbackFilesNoError(t *testing.T) {
	h, _ := makeHandlers(t)
	// No agent.md or prompt.md created — should not error, just use defaults.
	_, err := h.resolvePrompt("task")
	if err != nil {
		t.Fatalf("missing fallback files should not cause error, got: %v", err)
	}
}

// TestBootstrapPaths_PrefersMemoryDir verifies that bootstrapPaths returns
// paths from the memory dir when agent.md/prompt.md exist there.
func TestBootstrapPaths_PrefersMemoryDir(t *testing.T) {
	h, dir := makeHandlers(t)
	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create agent.md and prompt.md in the memory dir
	os.WriteFile(filepath.Join(memDir, "agent.md"), []byte("MEM AGENT"), 0644)
	os.WriteFile(filepath.Join(memDir, "prompt.md"), []byte("MEM PROMPT"), 0644)
	// Also create bare fallback files in CWD — should NOT be used
	os.WriteFile(filepath.Join(dir, "agent.md"), []byte("CWD AGENT"), 0644)
	os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("CWD PROMPT"), 0644)

	got, err := h.resolvePrompt("task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "MEM AGENT") {
		t.Error("expected memory dir agent.md to be used")
	}
	if strings.Contains(got, "CWD AGENT") {
		t.Error("CWD agent.md should not be used when memory dir has one")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
