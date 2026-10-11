package agentbox

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ab "github.com/tendant/agentbox/pkg/agentbox"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// fakeAB records what the adapter asks agentbox for.
type fakeAB struct {
	caps  ab.Capabilities
	specs []ab.Spec
	cmds  []ab.Command
}

func (f *fakeAB) Capabilities(context.Context) (ab.Capabilities, error) { return f.caps, nil }
func (f *fakeAB) Create(_ context.Context, s ab.Spec) (ab.Sandbox, error) {
	f.specs = append(f.specs, s)
	return &fakeSB{f: f}, nil
}

type fakeSB struct{ f *fakeAB }

func (s *fakeSB) ID() string { return "fake" }
func (s *fakeSB) Exec(context.Context, ab.Command) (ab.Result, error) {
	return ab.Result{}, nil
}
func (s *fakeSB) Close(context.Context) error { return nil }
func (s *fakeSB) Cmd(_ context.Context, c ab.Command) (*exec.Cmd, func(), error) {
	s.f.cmds = append(s.f.cmds, c)
	return exec.Command("true"), func() {}, nil
}

func roots(t *testing.T) map[string]string {
	d := t.TempDir()
	m := map[string]string{sandbox.RootWorkspace: filepath.Join(d, "ws"), sandbox.RootHome: filepath.Join(d, "home"), sandbox.RootTmp: filepath.Join(d, "tmp")}
	for _, p := range m {
		mkdir(t, p)
	}
	return m
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := exec.Command("mkdir", "-p", p).Run(); err != nil {
		t.Fatal(err)
	}
}

func spec() sandbox.SandboxSpec {
	return sandbox.SandboxSpec{Version: 1,
		Filesystem: sandbox.FilesystemPolicy{
			Grants: []sandbox.PathGrant{{Path: sandbox.RootWorkspace, Access: sandbox.ReadWrite},
				{Path: sandbox.RootTmp, Access: sandbox.ReadWrite}, {Path: sandbox.RootHome, Access: sandbox.ReadOnly}},
			Deny: []string{sandbox.RootHome + "/.ssh"}},
		Network:   sandbox.NetworkPolicy{Egress: sandbox.EgressNone},
		Resources: sandbox.ResourcePolicy{MemoryBytes: 1 << 30, PIDs: 256, TimeoutSec: 60},
	}
}

func TestCheck(t *testing.T) {
	f := &fakeAB{caps: ab.Capabilities{Rootless: true, NetworkNone: true, PIDLimit: true, MemoryLimit: true, CPULimit: true}}
	b := New(Config{Roots: roots(t), Backend: f})
	res, err := b.Check(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps) != 0 {
		t.Errorf("gaps for a no-network spec: %+v", res.Gaps)
	}
	for _, c := range []sandbox.CapabilityID{sandbox.CapFSReadScope, sandbox.CapFSDeny, sandbox.CapNetworkDirectNone,
		sandbox.CapResourceMemory, sandbox.CapResourcePIDs, sandbox.CapKernelIsolation} {
		if !slices.Contains(res.Enforced, c) {
			t.Errorf("%s not enforced: %v", c, res.Enforced)
		}
	}
	// Outbound network is a gap, said plainly.
	s := spec()
	s.Network.Egress = sandbox.EgressOutbound
	res, _ = b.Check(context.Background(), s)
	if len(res.Gaps) != 1 || res.Gaps[0].Capability != sandbox.CapNetworkOutbound || !strings.Contains(res.Gaps[0].Reason, "no network") {
		t.Errorf("outbound: %+v", res.Gaps)
	}
	// Without a scope, a memory limit is a gap.
	f2 := &fakeAB{caps: ab.Capabilities{Rootless: true, PIDLimit: true}}
	res, _ = New(Config{Roots: roots(t), Backend: f2}).Check(context.Background(), spec())
	if len(res.Gaps) != 1 || res.Gaps[0].Capability != sandbox.CapResourceMemory {
		t.Errorf("no scope: %+v", res.Gaps)
	}
	// No rootless runsc: an error, not a run.
	if _, err := New(Config{Roots: roots(t), Backend: &fakeAB{caps: ab.Capabilities{Notes: []string{"no profile"}}}}).Check(context.Background(), spec()); err == nil || !strings.Contains(err.Error(), "no profile") {
		t.Errorf("no rootless: %v", err)
	}
}

func TestPrepareMapsPolicy(t *testing.T) {
	f := &fakeAB{caps: ab.Capabilities{Rootless: true}}
	r := roots(t)
	extra := filepath.Join(t.TempDir(), "memory")
	mkdir(t, extra)
	mkdir(t, filepath.Join(r[sandbox.RootHome], ".ssh"))
	b := New(Config{Roots: r, Backend: f, Readable: []string{extra, filepath.Join(r[sandbox.RootHome], "sub")},
		ReadDeny: []string{"/srv/runner/.env"},
		Routes:   map[string]string{"127.0.0.1:4801": "/run/arm/anthropic.sock"}})
	ps, err := b.Prepare(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}
	s := f.specs[0]
	if s.Workspace != r[sandbox.RootWorkspace] {
		t.Errorf("workspace %q", s.Workspace)
	}
	// Grants at their own host paths; Readable read-only; nothing twice.
	if !slices.Contains(s.Writable, ab.Mount{Source: r[sandbox.RootWorkspace]}) || !slices.Contains(s.Writable, ab.Mount{Source: r[sandbox.RootTmp]}) {
		t.Errorf("writable %v", s.Writable)
	}
	if !slices.Contains(s.ReadOnly, ab.Mount{Source: r[sandbox.RootHome]}) || !slices.Contains(s.ReadOnly, ab.Mount{Source: extra}) {
		t.Errorf("read-only %v", s.ReadOnly)
	}
	if len(s.ReadOnly) != 2 {
		t.Errorf("a Readable path inside a grant must not be mounted again: %v", s.ReadOnly)
	}
	if !slices.Contains(s.Deny, filepath.Join(r[sandbox.RootHome], ".ssh")) || !slices.Contains(s.Deny, "/srv/runner/.env") {
		t.Errorf("deny %v", s.Deny)
	}
	if s.Limits.MemoryBytes != 1<<30 || s.Limits.PIDs != 256 || s.Lifetime == 0 {
		t.Errorf("limits %+v lifetime %v", s.Limits, s.Lifetime)
	}
	if !slices.Equal(s.ModelSockets, []string{"/run/arm/anthropic.sock"}) {
		t.Errorf("sockets %v", s.ModelSockets)
	}

	// Commands go through the forwarder (a copy of this binary in the tmp
	// root), with the logical dir mapped to its host path.
	c, err := ps.(sandbox.Commander).Command(context.Background(), sandbox.ProcSpec{Args: []string{"claude", "-p"}, Dir: sandbox.RootWorkspace, Env: []string{"A=1"}})
	if err != nil || c == nil {
		t.Fatal(err)
	}
	got := f.cmds[0]
	if got.Dir != r[sandbox.RootWorkspace] || !slices.Equal(got.Env, []string{"A=1"}) {
		t.Errorf("command %+v", got)
	}
	want := []string{filepath.Join(r[sandbox.RootTmp], ".agent-runner-fwd"), fwdMode, "127.0.0.1:4801=/run/agentbox/anthropic.sock", "--", "claude", "-p"}
	if !slices.Equal(got.Args, want) {
		t.Errorf("args %v\nwant %v", got.Args, want)
	}
	if err := ps.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
}
