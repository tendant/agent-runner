package chatcmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/botcommon"
	"github.com/agent-runner/agent-runner/internal/config"
)

// cmdTestEnv is a self-contained fixture for Commander tests: a config
// sandboxed to temp dirs and a fake Runtime backed by a real agent.Manager.
type cmdTestEnv struct {
	cfg          *config.Config
	rt           *fakeRuntime
	repoCacheDir string
}

// fakeRuntime implements Runtime without the api package.
type fakeRuntime struct {
	cfg     *config.Config
	mgr     *agent.Manager
	starter botcommon.AgentStarter
}

func (r *fakeRuntime) BootstrapPaths() (string, string) {
	systemPrompt := r.cfg.Agent.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = filepath.Join(r.cfg.MemoryDir, "agent.md")
	}
	promptFile := r.cfg.Agent.PromptFile
	if promptFile == "" {
		promptFile = filepath.Join(r.cfg.MemoryDir, "prompt.md")
	}
	return systemPrompt, promptFile
}
func (r *fakeRuntime) AgentManager() *agent.Manager { return r.mgr }
func (r *fakeRuntime) RefreshRuntime()              {}
func (r *fakeRuntime) AgentStarter() botcommon.AgentStarter {
	return r.starter
}

// fakeStarter satisfies botcommon.AgentStarter for command-initiated sessions.
type fakeStarter struct{}

func (fakeStarter) StartAgent(message, source, _ string) (string, error) {
	return "agent-test-fake", nil
}
func (fakeStarter) GetAgentSession(sessionID string) (*agent.Session, bool) {
	return nil, false
}

func setupTestEnv(t *testing.T) *cmdTestEnv {
	t.Helper()
	dir := t.TempDir()

	// Pin DATA_DIR so config.ReloadFromEnv triggered by /set resolves inside
	// the sandbox — never the real ~/.agent-runner.
	t.Setenv("DATA_DIR", dir)

	repoCacheDir := filepath.Join(dir, "repo-cache")
	os.MkdirAll(repoCacheDir, 0755)

	cfg := config.DefaultConfig()
	cfg.RepoCacheRoot = repoCacheDir
	cfg.LogsRoot = filepath.Join(dir, "logs")
	cfg.TmpRoot = filepath.Join(dir, "tmp")
	// Relative so tests that chdir into a temp CWD (withTempCWD) get an
	// isolated memory dir.
	cfg.MemoryDir = "memory"

	mgr := agent.NewManager(3600, 10)
	t.Cleanup(mgr.Stop)

	rt := &fakeRuntime{cfg: cfg, mgr: mgr, starter: fakeStarter{}}
	return &cmdTestEnv{cfg: cfg, rt: rt, repoCacheDir: repoCacheDir}
}
