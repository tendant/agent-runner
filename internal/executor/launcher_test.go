package executor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

type fakeBackend struct {
	gaps     []sandbox.Gap
	prepared int
	destroys *int
	got      sandbox.ProcSpec
}

func (f *fakeBackend) Check(context.Context, sandbox.SandboxSpec) (sandbox.CheckResult, error) {
	return sandbox.CheckResult{Backend: "fake", Gaps: f.gaps}, nil
}
func (f *fakeBackend) Prepare(context.Context, sandbox.SandboxSpec) (sandbox.PreparedSandbox, error) {
	f.prepared++
	return fakePrepared{f}, nil
}

type fakePrepared struct{ f *fakeBackend }

func (fakePrepared) Run(context.Context, sandbox.ProcSpec) (int, error) { return 0, nil }
func (p fakePrepared) Destroy(context.Context) error                    { *p.f.destroys++; return nil }
func (p fakePrepared) Command(ctx context.Context, ps sandbox.ProcSpec) (*exec.Cmd, error) {
	p.f.got = ps
	return exec.CommandContext(ctx, "true"), nil
}

func TestHostLauncherOverlaysEnv(t *testing.T) {
	cmd, release, err := HostLauncher{}.Command(context.Background(), LaunchSpec{Name: "echo", Args: []string{"x"}, Dir: "/tmp", ExtraEnv: []string{"A=1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if cmd.Dir != "/tmp" || cmd.Env[len(cmd.Env)-1] != "A=1" || len(cmd.Env) < 2 {
		t.Errorf("%+v", cmd)
	}
	cmd, _, _ = HostLauncher{}.Command(context.Background(), LaunchSpec{Name: "echo"})
	if cmd.Env != nil {
		t.Error("no overlay must inherit env (nil)")
	}
}

func TestSandboxLauncherStrictRejectsAndNeverFallsBack(t *testing.T) {
	d := 0
	fb := &fakeBackend{gaps: []sandbox.Gap{{Capability: sandbox.CapResourceMemory}}, destroys: &d}
	l := &SandboxLauncher{Backend: func(string) sandbox.Backend { return fb }, Mode: sandbox.ModeStrict}
	cmd, _, err := l.Command(context.Background(), LaunchSpec{Name: "claude", Dir: "/w"})
	if err == nil || cmd != nil {
		t.Fatalf("strict with gaps must reject, got cmd=%v err=%v", cmd, err)
	}
	if fb.prepared != 0 {
		t.Error("must not prepare when rejected")
	}
	// Mode off is a programming error, not a silent host run.
	l.Mode = sandbox.ModeOff
	if cmd, _, err := l.Command(context.Background(), LaunchSpec{Name: "claude"}); err == nil || cmd != nil {
		t.Error("mode off must not launch through SandboxLauncher")
	}
}

func TestSandboxLauncherPermissiveEnvAndRelease(t *testing.T) {
	os.Setenv("ALLOWED_VAR", "yes")
	os.Setenv("SECRET_VAR", "no")
	defer os.Unsetenv("ALLOWED_VAR")
	defer os.Unsetenv("SECRET_VAR")
	d := 0
	fb := &fakeBackend{gaps: []sandbox.Gap{{Capability: sandbox.CapResourceMemory}}, destroys: &d}
	var checked int
	l := &SandboxLauncher{Backend: func(string) sandbox.Backend { return fb }, Mode: sandbox.ModePermissive,
		EnvAllow: []string{"ALLOWED_VAR"}, OnCheck: func(sandbox.CheckResult, error) { checked++ }}
	cmd, release, err := l.Command(context.Background(), LaunchSpec{Name: "claude", Args: []string{"--print"}, Dir: "/w", ExtraEnv: []string{"X=1"}})
	if err != nil || cmd == nil {
		t.Fatal(err)
	}
	got := strings.Join(fb.got.Env, "\n")
	if !strings.Contains(got, "ALLOWED_VAR=yes") || !strings.Contains(got, "X=1") || strings.Contains(got, "SECRET_VAR") {
		t.Errorf("env: %s", got)
	}
	if fb.got.Args[0] != "claude" || fb.got.Dir != sandbox.RootWorkspace || checked != 1 {
		t.Errorf("%+v checked=%d", fb.got, checked)
	}
	release()
	if d != 1 {
		t.Error("release must destroy the sandbox")
	}
}

func TestSandboxLauncherPerCLIEnvAndForcedEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok")
	d := 0
	fb := &fakeBackend{destroys: &d}
	l := &SandboxLauncher{Backend: func(string) sandbox.Backend { return fb }, Mode: sandbox.ModePermissive,
		CLIEnvAllow: map[string][]string{"claude": {"CLAUDE_CODE_OAUTH_TOKEN"}},
		EnvForce:    []string{"CLAUDE_CONFIG_DIR=/sandbox/home/.claude"}}
	env := func(name string) string {
		_, release, err := l.Command(context.Background(), LaunchSpec{Name: name, Dir: "/w",
			ExtraEnv: []string{"CLAUDE_CONFIG_DIR=/runner/agent-home/claude"}})
		if err != nil {
			t.Fatal(err)
		}
		release()
		return strings.Join(fb.got.Env, "\n")
	}

	got := env("/usr/local/bin/claude")
	if !strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN=tok") {
		t.Errorf("claude did not get its token: %s", got)
	}
	// The forced value comes last, so it wins over the executor's overlay.
	if i, j := strings.LastIndex(got, "CLAUDE_CONFIG_DIR=/sandbox/home/.claude"), strings.LastIndex(got, "CLAUDE_CONFIG_DIR=/runner"); i < j {
		t.Errorf("forced env must come after the overlay: %s", got)
	}
	if got := env("codex"); strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("another CLI got claude's token: %s", got)
	}
}
