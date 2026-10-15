package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/repos"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
)

type nopSandbox struct{}

func (nopSandbox) Check(context.Context, sandbox.SandboxSpec) (sandbox.CheckResult, error) {
	return sandbox.CheckResult{Backend: "fake"}, nil
}
func (nopSandbox) Prepare(context.Context, sandbox.SandboxSpec) (sandbox.PreparedSandbox, error) {
	return nopPrepared{}, nil
}

type nopPrepared struct{}

func (nopPrepared) Run(context.Context, sandbox.ProcSpec) (int, error) { return 0, nil }
func (nopPrepared) Destroy(context.Context) error                      { return nil }
func (nopPrepared) Command(ctx context.Context, _ sandbox.ProcSpec) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "true"), nil
}

// jobExecutor stands in for the agent: it records whether its launch was
// confined, edits a file, and rewrites the repo's git config.
type jobExecutor struct {
	confined  *bool
	elsewhere string
}

func (e jobExecutor) run(ctx context.Context, ws string) (*executor.ExecutionResult, string, error) {
	_, *e.confined = executor.LauncherFrom(ctx)
	os.MkdirAll(filepath.Join(ws, "src"), 0o755)
	os.WriteFile(filepath.Join(ws, "src", "x.txt"), []byte("x\n"), 0o644)
	exec.Command("git", "-C", ws, "remote", "set-url", "origin", e.elsewhere).Run()
	return &executor.ExecutionResult{Output: "done"}, "", nil
}
func (e jobExecutor) Execute(ctx context.Context, ws, _ string) (*executor.ExecutionResult, error) {
	r, _, err := e.run(ctx, ws)
	return r, err
}
func (e jobExecutor) ExecuteWithSystemPrompt(ctx context.Context, ws, _, _ string) (*executor.ExecutionResult, error) {
	r, _, err := e.run(ctx, ws)
	return r, err
}
func (e jobExecutor) ExecuteWithLog(ctx context.Context, ws, _ string) (*executor.ExecutionResult, string, error) {
	return e.run(ctx, ws)
}
func (e jobExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, ws, _, _ string) (*executor.ExecutionResult, string, error) {
	return e.run(ctx, ws)
}

// A one-shot job runs its agent in the sandbox when it is on, and pushes
// to the registered repo whatever the agent did to the checkout's config.
func TestJobRunsSandboxed(t *testing.T) {
	dir := t.TempDir()
	repos.AllowLocalPaths = true
	bare := filepath.Join(dir, "site.git")
	src := filepath.Join(dir, "src")
	for _, args := range [][]string{{"init", "-q", "--bare", "-b", "main", bare}, {"clone", "-q", bare, src},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init"}, {"-C", src, "push", "-q", "origin", "HEAD"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	elsewhere := filepath.Join(dir, "elsewhere.git")
	exec.Command("git", "init", "-q", "--bare", elsewhere).Run()

	cfg := config.DefaultConfig()
	cfg.RepoCacheRoot = filepath.Join(dir, "cache")
	cfg.LogsRoot = filepath.Join(dir, "logs")
	cfg.TmpRoot = filepath.Join(dir, "tmp")
	cfg.ReposFile = filepath.Join(dir, "repos.json")
	cfg.SecretsFile = filepath.Join(dir, "secrets.json")
	cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds = 1, 0
	repos.New(cfg.ReposFile).Put(repos.Repo{Name: "site", URL: bare, Push: true})

	var confined bool
	jm := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	t.Cleanup(jm.Stop)
	am := agent.NewManager(3600, 10, 1)
	t.Cleanup(am.Stop)
	h := NewHandlers(cfg, jm, am, git.NewOperations(1, 0), jobExecutor{confined: &confined, elsewhere: elsewhere},
		executor.NewValidator(nil, false), executor.NewWorkspaceManager(cfg.TmpRoot, 60), logging.NewRunLogger(cfg.LogsRoot))
	rt, err := sandboxrt.New(sandboxrt.Config{Mode: sandbox.ModePermissive, StateDir: filepath.Join(dir, "sbx"), Instance: "t",
		NewBackend: func(map[string]string) sandbox.Backend { return nopSandbox{} }})
	if err != nil {
		t.Fatal(err)
	}
	h.sandbox = rt

	w := postJSON(h.HandleRun, map[string]any{"project": "site", "instruction": "add x", "paths": []string{"src/"}})
	if w.Code != 202 {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	for i := 0; i < 100; i++ {
		if out, _ := exec.Command("git", "-C", bare, "ls-tree", "-r", "--name-only", "main").Output(); strings.Contains(string(out), "src/x.txt") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !confined {
		t.Error("the job's agent must be launched through the sandbox")
	}
	if out, _ := exec.Command("git", "-C", bare, "ls-tree", "-r", "--name-only", "main").Output(); !strings.Contains(string(out), "src/x.txt") {
		t.Errorf("the job's commit must reach the registered repo: %s", out)
	}
	if out, _ := exec.Command("git", "-C", elsewhere, "for-each-ref").Output(); len(out) != 0 {
		t.Errorf("nothing may be pushed to the URL the agent planted: %s", out)
	}
}
