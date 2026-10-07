package api

import (
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
