package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// LaunchSpec describes an agent-CLI process to start. Dir is a host path.
type LaunchSpec struct {
	Name     string   // executable ("claude", "codex", ...)
	Args     []string // arguments
	Dir      string   // working directory (host path of the workspace)
	ExtraEnv []string // KEY=VALUE overlay requested by the executor
}

// Launcher builds the *exec.Cmd for an agent CLI. Every agent-CLI start goes
// through it so a sandbox can confine the whole process tree (the CLI and every
// tool it runs). The returned release func must be called after the command has
// finished (Wait returned or Start failed); it tears the sandbox down.
//
// With a SandboxLauncher there is no host fallback: if the sandbox cannot be
// prepared the launch fails.
type Launcher interface {
	Command(ctx context.Context, s LaunchSpec) (cmd *exec.Cmd, release func(), err error)
}

// LauncherSetter is implemented by executors that accept a Launcher.
type LauncherSetter interface {
	SetLauncher(Launcher)
}

// HostLauncher runs the CLI directly on the host (sandbox mode "off").
type HostLauncher struct{}

func (HostLauncher) Command(ctx context.Context, s LaunchSpec) (*exec.Cmd, func(), error) {
	cmd := exec.CommandContext(ctx, s.Name, s.Args...)
	cmd.Dir = s.Dir
	if len(s.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), s.ExtraEnv...)
	}
	return cmd, func() {}, nil
}

// launcherOrHost returns l, or HostLauncher when unset.
func launcherOrHost(l Launcher) Launcher {
	if l == nil {
		return HostLauncher{}
	}
	return l
}

// SandboxLauncher confines every launch with a sandbox.Backend.
type SandboxLauncher struct {
	// Backend builds the backend for one workspace (maps /workspace to dir).
	Backend func(workspaceDir string) sandbox.Backend
	// Spec is the fully resolved policy for the run.
	Spec sandbox.SandboxSpec
	// Mode is strict or permissive; ModeOff must use HostLauncher instead.
	Mode sandbox.Mode
	// EnvAllow names host environment variables copied into the sandbox
	// (interim until the model proxy keeps provider credentials outside).
	EnvAllow []string
	// OnCheck, if set, receives every check result (for events/logging).
	OnCheck func(sandbox.CheckResult, error)
}

func (l *SandboxLauncher) Command(ctx context.Context, s LaunchSpec) (*exec.Cmd, func(), error) {
	if l.Mode == sandbox.ModeOff || l.Mode == "" {
		return nil, nil, errors.New("sandbox launcher: mode off must use HostLauncher")
	}
	b := l.Backend(s.Dir)
	res, err := b.Check(ctx, l.Spec)
	if l.OnCheck != nil {
		l.OnCheck(res, err)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox check: %w", err)
	}
	if err := sandbox.Admit(l.Mode, res); err != nil {
		return nil, nil, err
	}
	ps, err := b.Prepare(ctx, l.Spec)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox prepare: %w", err)
	}
	cmdr, ok := ps.(sandbox.Commander)
	if !ok {
		_ = ps.Destroy(ctx)
		return nil, nil, errors.New("sandbox backend cannot provide commands")
	}
	cmd, err := cmdr.Command(ctx, sandbox.ProcSpec{
		Args: append([]string{s.Name}, s.Args...),
		Dir:  sandbox.RootWorkspace,
		Env:  l.env(s),
	})
	if err != nil {
		_ = ps.Destroy(ctx)
		return nil, nil, err
	}
	return cmd, func() { _ = ps.Destroy(context.Background()) }, nil
}

// env builds the complete sandbox environment: a minimal base, allowlisted
// host variables, then the executor's overlay.
func (l *SandboxLauncher) env(s LaunchSpec) []string {
	env := []string{"LANG=C.UTF-8", "TERM=dumb"}
	for _, k := range []string{"PATH", "HOME", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range l.EnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, s.ExtraEnv...)
}
