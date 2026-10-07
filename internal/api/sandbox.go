package api

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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
		PrivatePaths:  sandboxPrivatePaths(cfg),
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

// sandboxPrivatePaths lists the runner's own files a sandboxed agent must not
// read: its .env files (bot tokens, API keys, git tokens) and its state, logs,
// outputs, workspaces and repo cache. The memory dir and uploads stay
// readable: prompts point the agent at memory, and uploaded files reach it by
// path.
func sandboxPrivatePaths(cfg *config.Config) []string {
	paths := []string{cfg.StateRoot, cfg.TmpRoot, cfg.LogsRoot, cfg.OutputsRoot, cfg.RepoCacheRoot}
	for _, dir := range []string{cfg.ProjectDir, cfg.DataDir} {
		if dir == "" {
			continue
		}
		// Listed even before they exist (/set creates .env.local later): each
		// run checks the list again.
		paths = append(paths, filepath.Join(dir, ".env"), filepath.Join(dir, ".env.local"))
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if n := e.Name(); strings.HasPrefix(n, ".env.") && n != ".env.local" {
				paths = append(paths, filepath.Join(dir, n))
			}
		}
	}
	return paths
}
