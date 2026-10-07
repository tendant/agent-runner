package conformance

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// unsafeHost runs commands directly on the host: the negative control. The
// suite must catch that it enforces nothing.
type unsafeHost struct{ env Env }

func (u unsafeHost) Check(context.Context, sandbox.SandboxSpec) (sandbox.CheckResult, error) {
	return sandbox.CheckResult{Backend: "unsafe-host"}, nil
}
func (u unsafeHost) Prepare(context.Context, sandbox.SandboxSpec) (sandbox.PreparedSandbox, error) {
	return unsafeRun{u.env}, nil
}

type unsafeRun struct{ env Env }

func (u unsafeRun) Destroy(context.Context) error { return nil }
func (u unsafeRun) Run(ctx context.Context, p sandbox.ProcSpec) (int, error) {
	cmd := exec.CommandContext(ctx, p.Args[0], p.Args[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = u.env.Host(p.Dir), p.Env, p.Stdout, p.Stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}

func TestSuiteDetectsUnenforcedBackend(t *testing.T) {
	rep, err := Run(context.Background(), "unsafe-host", func(e Env) sandbox.Backend { return unsafeHost{e} })
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fs.ssh_unreadable", "fs.symlink_escape", "fs.no_write_outside_workspace", "fs.virtual_paths", "net.outbound_connect"} {
		got := rep.Results[id]
		want := Fail
		if id == "net.outbound_connect" {
			want = Pass // positive control: the host can connect
		}
		if got.Status != want {
			t.Errorf("%s: %+v, want %s", id, got, want)
		}
	}
	for _, c := range []sandbox.CapabilityID{sandbox.CapFSDeny, sandbox.CapFSWriteScope, sandbox.CapFSPathVirtualization, sandbox.CapNetworkRestrictedEgress, sandbox.CapKernelIsolation} {
		if rep.Proven(c) {
			t.Errorf("unenforced backend proven for %s", c)
		}
	}
}

func TestMissing(t *testing.T) {
	rep := Report{Results: map[string]Result{"fs.ssh_unreadable": {Status: Pass}, "fs.symlink_escape": {Status: Pass}, "net.none_loopback": {Status: Fail}}}
	got := rep.Missing([]sandbox.CapabilityID{sandbox.CapFSDeny, sandbox.CapNetworkDirectNone, sandbox.CapKernelIsolation})
	if len(got) != 2 || got[0] != sandbox.CapNetworkDirectNone || got[1] != sandbox.CapKernelIsolation {
		t.Errorf("%v", got)
	}
}
