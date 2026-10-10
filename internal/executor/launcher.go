package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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

type launcherKey struct{}

// WithLauncher returns a context whose agent-CLI launches (planner, reviewer,
// session, fallbacks: everything started under it) go through l. Setting it
// once at the top of a run confines every CLI the run starts, without mutating
// the shared executors.
func WithLauncher(ctx context.Context, l Launcher) context.Context {
	return context.WithValue(ctx, launcherKey{}, l)
}

// resolveLauncher prefers the context's launcher, then the executor's own,
// then the host.
func resolveLauncher(ctx context.Context, l Launcher) Launcher {
	if cl, ok := ctx.Value(launcherKey{}).(Launcher); ok && cl != nil {
		return cl
	}
	if l != nil {
		return l
	}
	return HostLauncher{}
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
	// CLIEnvAllow names host environment variables copied in only for one
	// CLI, keyed by its binary name (e.g. "claude": CLAUDE_CODE_OAUTH_TOKEN),
	// so a credential reaches the CLI it is for and no other.
	CLIEnvAllow map[string][]string
	// CLIEnv sets KEY=VALUE pairs for one CLI only, keyed like CLIEnvAllow
	// (e.g. "claude": the per-run model proxy's URL and token).
	CLIEnv map[string][]string
	// EnvOverride is applied after the base env (e.g. HOME/TMPDIR inside the
	// sandbox) and before the executor's own overlay.
	EnvOverride []string
	// EnvForce is applied last, over the executor's overlay: settings the
	// sandbox must own, such as a CLI config dir inside the sandbox home
	// (an overlay's AGENT_ISOLATED config dir is outside it and read-only).
	EnvForce []string
	// ScratchDir is a directory the sandboxed CLI can write and the runner
	// can read (the run's tmp): where an executor puts a file the CLI writes
	// its output to. The host's temp dir is not writable from the sandbox.
	ScratchDir string
	// Tag labels every sandboxed process (visible in argv) so an external
	// supervisor can find the process group of a run.
	Tag string
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
		Tag:  l.Tag,
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
	for _, k := range []string{"PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range append(append([]string(nil), l.EnvAllow...), l.CLIEnvAllow[filepath.Base(s.Name)]...) {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, l.EnvOverride...)
	env = append(env, s.ExtraEnv...)
	env = append(env, l.CLIEnv[filepath.Base(s.Name)]...)
	return append(env, l.EnvForce...)
}

// LauncherFrom returns the launcher carried by ctx, if any.
func LauncherFrom(ctx context.Context) (Launcher, bool) {
	l, ok := ctx.Value(launcherKey{}).(Launcher)
	return l, ok && l != nil
}

// scratchDir is where an executor should create a file the CLI writes to:
// the sandbox's scratch dir when ctx carries a sandbox launcher, else ""
// (the host's temp dir).
func scratchDir(ctx context.Context, l Launcher) string {
	if sl, ok := resolveLauncher(ctx, l).(*SandboxLauncher); ok {
		return sl.ScratchDir
	}
	return ""
}
