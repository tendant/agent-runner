package api

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
)

func TestSandboxRuntimeConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	if newSandboxRuntime(cfg) != nil {
		t.Error("default must be off")
	}
	cfg.Agent.SandboxMode = "strict"
	cfg.StateRoot = t.TempDir()
	if rt := newSandboxRuntime(cfg); rt == nil || rt.Mode() != sandbox.ModeStrict {
		t.Error("strict")
	}
	cfg.Agent.SandboxMode = "yolo"
	rt := newSandboxRuntime(cfg)
	if rt == nil || rt.Mode() != sandbox.ModeStrict {
		t.Error("bad mode must yield a rejecting runtime, not host execution")
	}
}

func TestSandboxPrivatePaths(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProjectDir, cfg.DataDir = t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(cfg.ProjectDir, ".env.prod"), []byte("X=1"), 0o600)
	os.WriteFile(filepath.Join(cfg.ProjectDir, ".envrc"), []byte(""), 0o600)
	got := map[string]bool{}
	for _, p := range sandboxPrivatePaths(cfg) {
		got[p] = true
	}
	for _, want := range []string{
		cfg.StateRoot, cfg.TmpRoot, cfg.LogsRoot, cfg.OutputsRoot, cfg.RepoCacheRoot,
		filepath.Join(cfg.ProjectDir, ".env"), filepath.Join(cfg.ProjectDir, ".env.prod"),
		filepath.Join(cfg.DataDir, ".env.local"), // listed before /set creates it
	} {
		if !got[want] {
			t.Errorf("missing %s", want)
		}
	}
	// Agents are pointed at memory and uploads; those stay readable.
	for _, readable := range []string{cfg.MemoryDir, cfg.UploadsRoot, filepath.Join(cfg.ProjectDir, ".envrc")} {
		if got[readable] {
			t.Errorf("%s must stay readable", readable)
		}
	}
}

func TestSandboxCodexLogin(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-real")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := config.DefaultConfig()
	cfg.Agent.SandboxModelProxy = true
	dir := filepath.Join(t.TempDir(), "codex-login")
	os.MkdirAll(dir, 0o700)
	login := filepath.Join(dir, "auth.json")
	os.WriteFile(login, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","account_id":"x"}}`), 0o600)
	cfg.Agent.SandboxCodexLogin = login

	ch := sandboxModelChannels(cfg)[sandboxrt.ProviderOpenAI]
	if ch == nil || !ch.ChatGPT || ch.Credential == nil || ch.Upstream.Host != "chatgpt.com" || ch.AuthValue != "" {
		t.Fatalf("with a login: %+v", ch)
	}
	if !slices.Contains(sandboxPrivatePaths(cfg), dir) {
		t.Errorf("the login's directory must be hidden from the sandbox")
	}

	// Unusable logins fall back to the API key, if any.
	home, _ := os.UserHomeDir()
	for name, path := range map[string]string{"in home": filepath.Join(home, "auth.json"), "missing": filepath.Join(dir, "none.json")} {
		cfg.Agent.SandboxCodexLogin = path
		if ch := sandboxModelChannels(cfg)[sandboxrt.ProviderOpenAI]; ch == nil || ch.ChatGPT || ch.AuthValue != "Bearer sk-real" {
			t.Errorf("%s: %+v", name, ch)
		}
	}
	cfg.Agent.SandboxCodexLogin = filepath.Join(home, "auth.json")
	if slices.Contains(sandboxPrivatePaths(cfg), home) {
		t.Error("the home directory must never be hidden")
	}
}
