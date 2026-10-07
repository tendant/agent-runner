package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/sandbox"
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
