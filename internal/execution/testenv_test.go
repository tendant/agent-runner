package execution

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/llm"
	"github.com/agent-runner/agent-runner/internal/logging"
)

// engineTestEnv is a self-contained fixture: a real Engine over sandboxed
// dirs and swappable deps. The `handlers` field name is kept so the tests
// moved from the api package read unchanged.
type engineTestEnv struct {
	handlers *engineShim
}

// engineShim exposes the moved tests' historical spelling
// (env.handlers.executor = ..., env.handlers.agentManager, ...).
type engineShim struct {
	*Engine
	executor     executor.Executor
	agentManager *agent.Manager
}

func (s *engineShim) Executor() executor.Executor       { return s.executor }
func (s *engineShim) PlannerClient() llm.Client         { return nil }
func (s *engineShim) CuratorClient() llm.Client         { return nil }
func (s *engineShim) Notifier() Notifier                { return nil }
func (s *engineShim) WorkflowClient() WorkflowScheduler { return nil }
func (s *engineShim) BootstrapPaths() (string, string) {
	return filepath.Join(s.Engine.config.MemoryDir, "agent.md"), filepath.Join(s.Engine.config.MemoryDir, "prompt.md")
}

func setupTestEnv(t *testing.T) *engineTestEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	// Separate temp dirs for state that background goroutines keep writing
	// to after a test returns, so they don't race t.TempDir cleanup.
	tmpDir, err := os.MkdirTemp("", "execution-test-tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	logsDir, err := os.MkdirTemp("", "execution-test-logs-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(logsDir) })

	cfg := config.DefaultConfig()
	cfg.RepoCacheRoot = filepath.Join(dir, "repo-cache")
	cfg.LogsRoot = logsDir
	cfg.TmpRoot = tmpDir
	cfg.MemoryDir = filepath.Join(dir, "memory")
	cfg.OutputsRoot = filepath.Join(dir, "outputs")
	cfg.UploadsRoot = filepath.Join(dir, "uploads")
	os.MkdirAll(cfg.RepoCacheRoot, 0755)

	mgr := agent.NewManager(3600, 10)
	t.Cleanup(mgr.Stop)

	shim := &engineShim{
		executor:     executor.NewExecutor("claude", "", "", 0),
		agentManager: mgr,
	}
	shim.Engine = New(cfg, mgr, executor.NewWorkspaceManager(tmpDir, cfg.MaxRuntimeSeconds), logging.NewRunLogger(logsDir), shim)

	return &engineTestEnv{handlers: shim}
}
