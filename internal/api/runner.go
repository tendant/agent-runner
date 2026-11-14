package api

import (
	"path/filepath"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/execution"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
)

// NewRunner builds the engine of a session runner (`agent-runner
// run-session`, JOBS_DESIGN.md §5): the executor, planner and sandbox a
// session runs with, without the server, bots, journal or proposals service,
// which are the dispatcher's.
//
// The runner records its sandbox audit apart, in
// <AGENT_SANDBOX_AUDIT_DIR>-runners/<session>: a record directory has one
// writer, and the dispatcher's is the dispatcher's.
func NewRunner(cfg *config.Config, sessionID string) *execution.Engine {
	if dir := cfg.Agent.SandboxAuditDir; dir != "" {
		cfg.Agent.SandboxAuditDir = RunnerAuditDir(dir, sessionID)
	}
	agentManager := agent.NewManager(cfg.JobRetentionSeconds, 1, 1)
	exec := executor.NewExecutor(cfg.Agent.CLI, cfg.Agent.Provider, cfg.Agent.Model, cfg.Agent.MaxTurns)
	applyIsolation(cfg.Agent.Isolated, exec)
	h := NewHandlers(cfg,
		jobs.NewManager(cfg.JobRetentionSeconds, 1),
		agentManager,
		git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds),
		exec,
		executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles),
		executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds),
		logging.NewRunLogger(cfg.LogsRoot),
	)
	h.RefreshRuntime()
	h.execEngine.EnableRunnerMode(cfg.Agent.Proposals)
	return h.execEngine
}

// RunnerAuditDir is where a runner of sessionID records its sandbox runs,
// given the dispatcher's record directory.
//
// A dispatcher directory that is a symlink (into a host's shared audit root,
// which agent-gate reads) is followed, so the runners' records land beside
// the record itself there, not beside the link.
func RunnerAuditDir(dispatcherDir, sessionID string) string {
	dir := filepath.Clean(dispatcherDir)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Join(dir+"-runners", sessionID)
}
