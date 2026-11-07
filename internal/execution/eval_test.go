package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/repos"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

// workExecutor stands in for an agent that writes in its working directory.
type workExecutor struct {
	fn func(dir, prompt string) string
}

func (w workExecutor) Execute(_ context.Context, dir, in string) (*executor.ExecutionResult, error) {
	return &executor.ExecutionResult{Output: w.fn(dir, in)}, nil
}
func (w workExecutor) ExecuteWithSystemPrompt(ctx context.Context, d, sys, in string) (*executor.ExecutionResult, error) {
	return w.Execute(ctx, d, sys+"\n"+in)
}
func (w workExecutor) ExecuteWithLog(ctx context.Context, d, in string) (*executor.ExecutionResult, string, error) {
	r, err := w.Execute(ctx, d, in)
	return r, "", err
}
func (w workExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, d, sys, in string) (*executor.ExecutionResult, string, error) {
	r, err := w.Execute(ctx, d, sys+"\n"+in)
	return r, "", err
}

func evalEnv(t *testing.T, sandboxed bool) (*engineTestEnv, string) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	h.config.Agent.ReviewerEnabled = false
	if sandboxed {
		rt, err := sandboxrt.New(sandboxrt.Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "t",
			NewBackend: func(map[string]string) sandbox.Backend { return nopSandbox{} }})
		if err != nil {
			t.Fatal(err)
		}
		h.Engine.sandbox = rt
	}
	mem := h.config.MemoryDir
	os.MkdirAll(mem, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"}} {
		exec.Command("git", append([]string{"-C", mem}, args...)...).Run()
	}
	os.WriteFile(filepath.Join(mem, "agent.md"), []byte("REAL instructions mem={{MEMORY_DIR}}"), 0o644)
	os.WriteFile(filepath.Join(mem, "GOALS.md"), []byte("real goal"), 0o644)
	os.WriteFile(filepath.Join(mem, "decisions.md"), []byte("use go\n"), 0o644)
	exec.Command("git", "-C", mem, "add", "-A").Run()
	exec.Command("git", "-C", mem, "commit", "-qm", "init").Run()
	// A secret and a repo the agent may push itself: a replay gets neither.
	os.WriteFile(h.config.SecretsFile, []byte(`{"GH_TOKEN":{"name":"GH_TOKEN","value":"tok-123","description":"GitHub"}}`), 0o600)
	os.WriteFile(h.config.ReposFile, []byte(`[{"name":"app","url":"https://github.com/acme/app.git","secret":"GH_TOKEN","push":true}]`), 0o644)
	return env, mem
}

// A replay runs with the variant's instructions, is judged by its checks,
// and leaves nothing behind.
func TestEvalReplayKeepsNothing(t *testing.T) {
	env, mem := evalEnv(t, true)
	h := env.handlers
	var sawPrompt string
	h.executor = workExecutor{fn: func(dir, prompt string) string {
		if sawPrompt == "" {
			sawPrompt = prompt
		}
		os.WriteFile(filepath.Join(dir, "out.txt"), []byte("x"), 0o644)
		if m := regexp.MustCompile(`mem=(\S+)`).FindStringSubmatch(prompt); m != nil {
			os.WriteFile(filepath.Join(m[1], "decisions.md"), []byte("use go\nuse sqlite\n"), 0o644)
		}
		return "Done: ran gofmt first."
	}}
	session, err := h.agentManager.CreateSession("add out.txt", nil, "test", "", 1, 60)
	if err != nil {
		t.Fatal(err)
	}
	session.Source = "eval"
	session.Eval = &agent.EvalSpec{
		Overrides: map[string]string{"agent.md": "VARIANT instructions mem={{MEMORY_DIR}}", "GOALS.md": "variant goal", "skills/notes/SKILL.md": "# notes skill"},
		Checks: []agent.EvalCheck{
			{Kind: "file", Arg: "out.txt"}, {Kind: "file", Arg: "missing.txt"}, {Kind: "file", Arg: "../escape"},
			{Kind: "reply", Arg: "GOFMT"}, {Kind: "command", Arg: "true"},
		},
	}
	h.ExecuteAgent(session)

	if !strings.Contains(sawPrompt, "VARIANT instructions") || strings.Contains(sawPrompt, "REAL instructions") {
		t.Errorf("the replay did not use the variant's agent.md:\n%s", sawPrompt)
	}
	if !strings.Contains(sawPrompt, "variant goal") || strings.Contains(sawPrompt, "real goal") {
		t.Errorf("the replay did not use the variant's GOALS.md")
	}
	if strings.Contains(sawPrompt, "_proposals/") {
		t.Error("a replay was told it may propose")
	}
	if strings.Contains(sawPrompt, "push it yourself") || strings.Contains(sawPrompt, "$GH_TOKEN") {
		t.Error("a replay was offered a push or a secret")
	}
	snap := session.Snapshot()
	want := []bool{true, false, false, true, true}
	if len(snap.EvalResults) != len(want) {
		t.Fatalf("results %+v (status %s, error %s)", snap.EvalResults, snap.Status, snap.Error)
	}
	for i, w := range want {
		if snap.EvalResults[i].Pass != w {
			t.Errorf("check %d %+v: pass=%v, want %v", i, snap.EvalResults[i].Check, snap.EvalResults[i].Pass, w)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(mem, "decisions.md")); string(b) != "use go\n" {
		t.Errorf("a replay's memory change was kept: %q", b)
	}
	if out, _ := exec.Command("git", "-C", mem, "log", "--format=%s").Output(); strings.TrimSpace(string(out)) != "init" {
		t.Errorf("a replay committed to memory:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(mem, tmpl.DailyLogName())); !os.IsNotExist(err) {
		t.Error("a replay wrote the daily log")
	}
	if snap.WorkspacePath != "" {
		if _, err := os.Stat(snap.WorkspacePath); !os.IsNotExist(err) {
			t.Error("a replay's workspace was left")
		}
	}
}

func TestEvalCommandNeedsSandbox(t *testing.T) {
	env, _ := evalEnv(t, false)
	h := env.handlers
	h.executor = workExecutor{fn: func(string, string) string { return "ok" }}
	session, _ := h.agentManager.CreateSession("x", nil, "test", "", 1, 60)
	session.Source = "eval"
	session.Eval = &agent.EvalSpec{Checks: []agent.EvalCheck{{Kind: "command", Arg: "true"}}}
	h.ExecuteAgent(session)
	r := session.Snapshot().EvalResults
	if len(r) != 1 || r[0].Pass || !strings.Contains(r[0].Detail, "need a sandbox") {
		t.Fatalf("a command check ran without a sandbox: %+v", r)
	}
}

func TestEvalSourceWithoutSpecDoesNotRun(t *testing.T) {
	env, _ := evalEnv(t, false)
	h := env.handlers
	ran := false
	h.executor = workExecutor{fn: func(string, string) string { ran = true; return "ok" }}
	session, _ := h.agentManager.CreateSession("x", nil, "test", "", 1, 60)
	session.Source = "eval"
	h.ExecuteAgent(session)
	if ran || session.Snapshot().Status != agent.SessionStatusFailed {
		t.Fatalf("ran=%v status=%s", ran, session.Snapshot().Status)
	}
}

func TestEvalReposAreReadOnly(t *testing.T) {
	list := []repos.Repo{{Name: "app", URL: "https://github.com/acme/app.git", Secret: "GH_TOKEN", Push: true}}
	if !strings.Contains(runtimeInstructions(runtimeFacts{Repos: evalRepos(list, nil)}), "push it yourself") {
		t.Fatal("the ordinary prompt offers no self-push: this test proves nothing")
	}
	got := runtimeInstructions(runtimeFacts{Repos: evalRepos(list, &agent.EvalSpec{})})
	if strings.Contains(got, "push it yourself") || !strings.Contains(got, "read-only: never pushed") {
		t.Fatalf("a replay's repos:\n%s", got)
	}
}
