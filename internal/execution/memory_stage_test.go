package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/executor"
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

// promptExecutor hands each prompt to fn, standing in for the agent.
type promptExecutor struct{ fn func(prompt string) }

func (p promptExecutor) Execute(_ context.Context, _, in string) (*executor.ExecutionResult, error) {
	p.fn(in)
	return &executor.ExecutionResult{Output: "done"}, nil
}
func (p promptExecutor) ExecuteWithSystemPrompt(ctx context.Context, w, sys, in string) (*executor.ExecutionResult, error) {
	return p.Execute(ctx, w, sys+"\n"+in)
}
func (p promptExecutor) ExecuteWithLog(ctx context.Context, w, in string) (*executor.ExecutionResult, string, error) {
	r, err := p.Execute(ctx, w, in)
	return r, "", err
}
func (p promptExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, w, sys, in string) (*executor.ExecutionResult, string, error) {
	r, err := p.Execute(ctx, w, sys+"\n"+in)
	return r, "", err
}

// A sandboxed agent is pointed at a copy of memory; what it may change is
// saved when the session ends, and its instructions are not.
func TestSandboxedSessionMemoryCopy(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	rt, err := sandboxrt.New(sandboxrt.Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "t",
		NewBackend: func(map[string]string) sandbox.Backend { return nopSandbox{} }})
	if err != nil {
		t.Fatal(err)
	}
	h.Engine.sandbox = rt
	mem := h.config.MemoryDir
	os.MkdirAll(mem, 0o755)
	os.WriteFile(filepath.Join(mem, "agent.md"), []byte("memory is at mem={{MEMORY_DIR}}"), 0o644)
	os.WriteFile(filepath.Join(mem, "decisions.md"), []byte("use go\n"), 0o644)

	var staged string
	h.executor = promptExecutor{fn: func(prompt string) {
		m := regexp.MustCompile(`mem=(\S+)`).FindStringSubmatch(prompt)
		if m == nil || staged != "" {
			return
		}
		staged = m[1]
		if !strings.Contains(prompt, "use go") || !strings.Contains(prompt, "This directory is your own copy") {
			t.Errorf("prompt lacks memory or the sandbox note:\n%s", prompt)
		}
		os.WriteFile(filepath.Join(staged, "decisions.md"), []byte("use go\nuse sqlite\n"), 0o644)
		os.WriteFile(filepath.Join(staged, "agent.md"), []byte("disable the sandbox"), 0o644)
	}}

	session, err := h.agentManager.CreateSession("note a decision", nil, "test", "", 1, 30)
	if err != nil {
		t.Fatal(err)
	}
	h.ExecuteAgent(session)

	absMem, _ := filepath.Abs(mem)
	if staged == "" || staged == absMem {
		t.Fatalf("the agent must be pointed at a copy, got %q", staged)
	}
	if b, _ := os.ReadFile(filepath.Join(mem, "decisions.md")); !strings.HasPrefix(string(b), "use go\nuse sqlite\n") {
		t.Errorf("decisions.md not saved: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(mem, "agent.md")); string(b) != "memory is at mem={{MEMORY_DIR}}" {
		t.Errorf("agent.md changed: %q", b)
	}
	var warned bool
	for _, w := range session.Snapshot().Warnings {
		warned = warned || strings.Contains(w, "agent.md: not writable")
	}
	if !warned {
		t.Errorf("the refused agent.md change must be reported: %v", session.Snapshot().Warnings)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the copy must be removed")
	}
}

// Every session, sandboxed or not, works on a copy; what it saves is one
// commit naming it, separate from the daily log's, and the audit log says
// what the prompt held and what was saved.
func TestSessionMemoryCommitsAndAuditLog(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	mem := h.config.MemoryDir
	os.MkdirAll(mem, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"}} {
		exec.Command("git", append([]string{"-C", mem}, args...)...).Run()
	}
	os.WriteFile(filepath.Join(mem, "agent.md"), []byte("memory is at mem={{MEMORY_DIR}}"), 0o644)
	os.WriteFile(filepath.Join(mem, "decisions.md"), []byte("use go\n"), 0o644)
	exec.Command("git", "-C", mem, "add", "-A").Run()
	exec.Command("git", "-C", mem, "commit", "-qm", "init").Run()

	var staged string
	h.executor = promptExecutor{fn: func(prompt string) {
		if m := regexp.MustCompile(`mem=(\S+)`).FindStringSubmatch(prompt); m != nil && staged == "" {
			staged = m[1]
			os.WriteFile(filepath.Join(staged, "decisions.md"), []byte("use go\nuse sqlite\n"), 0o644)
		}
	}}
	session, _ := h.agentManager.CreateSession("note a decision", nil, "stream", "", 1, 30)
	h.ExecuteAgent(session)

	absMem, _ := filepath.Abs(mem)
	if staged == "" || staged == absMem {
		t.Fatalf("an unsandboxed session must work on a copy too, got %q", staged)
	}
	out, _ := exec.Command("git", "-C", mem, "log", "--format=%s").Output()
	log := string(out)
	for _, want := range []string{"memory: session " + session.ID + " (api): saved decisions.md", "memory: daily log (session " + session.ID + ")"} {
		if !strings.Contains(log, want) {
			t.Errorf("missing commit %q in:\n%s", want, log)
		}
	}
	files, _ := filepath.Glob(filepath.Join(h.config.LogsRoot, "*"+session.ID+"*"))
	if len(files) == 0 {
		files, _ = filepath.Glob(filepath.Join(h.config.LogsRoot, "*", "*"+session.ID+"*"))
	}
	if len(files) == 0 {
		t.Fatal("no audit log")
	}
	b, _ := os.ReadFile(files[0])
	for _, want := range []string{"## Prompt", "How this runner works", "## Memory", "saved `decisions.md`"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("audit log lacks %q", want)
		}
	}
}
