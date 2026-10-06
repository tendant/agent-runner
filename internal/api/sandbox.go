package api

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
)

// newSandboxRuntime builds the sandbox runtime from config. An invalid
// configuration yields a runtime that rejects every run rather than silently
// running agents on the host.
func newSandboxRuntime(cfg *config.Config) *sandboxrt.Runtime {
	mode := sandbox.Mode(cfg.Agent.SandboxMode)
	if mode == "" || mode == sandbox.ModeOff {
		return nil
	}
	host, _ := os.Hostname()
	rt, err := sandboxrt.New(sandboxrt.Config{
		Mode:          mode,
		StateDir:      filepath.Join(cfg.StateRoot, "sandbox"),
		Instance:      host,
		IsoboxBin:     cfg.Agent.SandboxIsobox,
		IsoboxBackend: cfg.Agent.SandboxBackend,
		PolicyFile:    cfg.Agent.SandboxPolicyFile,
		EvidenceFile:  cfg.Agent.SandboxEvidence,
		EnvAllow:      cfg.Agent.SandboxEnvAllow,
		OnEvent: func(e sandboxrt.Event) {
			// Names and ids only; never environment values or secrets.
			slog.Info(e.Kind, "run", e.RunID, "thread", e.ThreadID, "backend", e.Backend, "detail", e.Detail)
		},
	})
	if err != nil {
		slog.Error("sandbox misconfigured; all agent runs will be rejected", "mode", mode, "error", err)
		return sandboxrt.Failed(err)
	}
	slog.Info("sandbox enabled", "mode", mode, "backend", cfg.Agent.SandboxBackend)
	return rt
}
