package agentbox

import (
	"context"
	"fmt"
	"os"
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
	f := &fakeAB{caps: ab.Capabilities{Rootless: true, NetworkNone: true, PIDLimit: true, MemoryLimit: true, CPULimit: true, KernelIsolation: true, ModelSocket: true}}
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
	if len(res.Gaps) != 1 || res.Gaps[0].Capability != sandbox.CapNetworkOutbound || !strings.Contains(res.Gaps[0].Reason, "restricted") {
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
	f := &fakeAB{caps: ab.Capabilities{Rootless: true, ModelSocket: true, PIDLimit: true, MemoryLimit: true}}
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

// Restricted egress: a gateway per run on a private socket, routed at
// 127.0.0.1:3128, proxy variables for commands, gone with the sandbox.
func TestRestrictedEgress(t *testing.T) {
	f := &fakeAB{caps: ab.Capabilities{Rootless: true, PIDLimit: true, ModelSocket: true}}
	r := roots(t)
	b := New(Config{Roots: r, Backend: f})
	s := spec()
	s.Resources = sandbox.ResourcePolicy{}
	s.Network.Egress, s.Network.EgressAllow = sandbox.EgressRestricted, []string{"*"}
	res, _ := b.Check(context.Background(), s)
	if len(res.Gaps) != 0 {
		t.Errorf("restricted egress is enforced: %+v", res.Gaps)
	}
	ps, err := b.Prepare(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	sp := f.specs[0]
	var sock string
	for _, m := range sp.ModelSockets {
		if filepath.Base(m) == "egress.sock" {
			sock = m
		}
	}
	if sock == "" {
		t.Fatalf("no gateway socket mounted: %v", sp.ModelSockets)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("gateway socket: %v", err)
	}
	ps.(sandbox.Commander).Command(context.Background(), sandbox.ProcSpec{Args: []string{"git", "fetch"}})
	got := f.cmds[0]
	if !slices.Contains(got.Args, "127.0.0.1:3128=/run/agentbox/egress.sock") {
		t.Errorf("no egress route: %v", got.Args)
	}
	if !slices.Contains(got.Env, "HTTPS_PROXY=http://127.0.0.1:3128") || !slices.Contains(got.Env, "NO_PROXY=127.0.0.1,localhost") {
		t.Errorf("proxy env: %v", got.Env)
	}
	ps.Destroy(context.Background())
	if _, err := os.Stat(filepath.Dir(sock)); !os.IsNotExist(err) {
		t.Errorf("gateway dir left: %v", err)
	}
	// No egress: no gateway.
	f2 := &fakeAB{caps: f.caps}
	New(Config{Roots: r, Backend: f2}).Prepare(context.Background(), spec())
	if len(f2.specs[0].ModelSockets) != 0 {
		t.Errorf("egress none started a gateway: %v", f2.specs[0].ModelSockets)
	}
}

// Transparent egress: where the host supports agentbox's gateway network,
// the gateway gets every TCP connection; no proxy route or variables.
func TestTransparentEgress(t *testing.T) {
	caps := ab.Capabilities{Rootless: true, PIDLimit: true, Gateway: true}
	s := spec()
	s.Resources = sandbox.ResourcePolicy{}
	s.Network.Egress, s.Network.EgressAllow = sandbox.EgressRestricted, []string{"*"}

	f := &fakeAB{caps: caps}
	ps, err := New(Config{Roots: roots(t), Backend: f}).Prepare(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	sp := f.specs[0]
	if sp.Network.Mode != ab.NetworkGateway || filepath.Base(sp.Network.Gateway) != "egress.sock" {
		t.Errorf("network %+v", sp.Network)
	}
	if _, err := os.Stat(sp.Network.Gateway); err != nil {
		t.Errorf("gateway socket: %v", err)
	}
	if len(sp.ModelSockets) != 0 {
		t.Errorf("the gateway is not mounted in transparent mode: %v", sp.ModelSockets)
	}
	ps.(sandbox.Commander).Command(context.Background(), sandbox.ProcSpec{Args: []string{"git", "fetch"}})
	for _, e := range f.cmds[0].Env {
		if strings.HasPrefix(e, "HTTPS_PROXY=") {
			t.Errorf("proxy variables in transparent mode: %v", f.cmds[0].Env)
		}
	}
	if slices.Contains(f.cmds[0].Args, "127.0.0.1:3128=/run/agentbox/egress.sock") {
		t.Errorf("proxy route in transparent mode: %v", f.cmds[0].Args)
	}
	ps.Destroy(context.Background())

	// EgressMode proxy keeps the proxy path on such a host.
	f2 := &fakeAB{caps: caps}
	b2 := New(Config{Roots: roots(t), Backend: f2, EgressMode: EgressProxy})
	ps, _ = b2.Prepare(context.Background(), s)
	if f2.specs[0].Network.Mode == ab.NetworkGateway {
		t.Error("proxy mode used the gateway network")
	}
	ps.Destroy(context.Background())
	if res, _ := b2.Check(context.Background(), s); !slices.ContainsFunc(res.Caveats, func(c string) bool { return strings.Contains(c, "HTTPS_PROXY only") }) {
		t.Errorf("proxy mode must say what it misses: %v", res.Caveats)
	}

	// Required but unavailable: refused before anything runs.
	f3 := &fakeAB{caps: ab.Capabilities{Rootless: true, Notes: []string{"no nft"}}}
	if _, err := New(Config{Roots: roots(t), Backend: f3, EgressMode: EgressTransparent}).Check(context.Background(), s); err == nil || !strings.Contains(err.Error(), "no nft") {
		t.Errorf("transparent required: %v", err)
	}
}

// Seatbelt-like backends share the host's network stack: model proxies and
// the gateway on the host's loopback, reachable at exactly their ports;
// HTTPS_PROXY points at the gateway; limits the backend lacks are still
// asked for, so agentbox refuses the sandbox rather than run it uncapped.
func TestHostPortsBackend(t *testing.T) {
	f := &fakeAB{caps: ab.Capabilities{Backend: "seatbelt", Rootless: true, NetworkNone: true, HostPorts: true}}
	s := spec()
	s.Network.Egress, s.Network.EgressAllow = sandbox.EgressRestricted, []string{"*"}
	b := New(Config{Roots: roots(t), Backend: f, HostPorts: []int{4801}})
	res, err := b.Check(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Backend != "agentbox:seatbelt" || slices.Contains(res.Enforced, sandbox.CapKernelIsolation) || slices.Contains(res.Enforced, sandbox.CapResourceMemory) {
		t.Errorf("check %+v", res)
	}
	ps, err := b.Prepare(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Destroy(context.Background())
	sp := f.specs[0]
	if len(sp.HostPorts) != 2 || sp.HostPorts[0] != 4801 || len(sp.ModelSockets) != 0 || sp.Network.Mode == ab.NetworkGateway {
		t.Errorf("spec %+v", sp)
	}
	if sp.Limits.MemoryBytes != 1<<30 || sp.Limits.PIDs != 256 {
		t.Errorf("requested limits were dropped: %+v", sp.Limits)
	}
	ps.(sandbox.Commander).Command(context.Background(), sandbox.ProcSpec{Args: []string{"git", "fetch"}})
	want := fmt.Sprintf("HTTPS_PROXY=http://127.0.0.1:%d", sp.HostPorts[1])
	if !slices.Contains(f.cmds[0].Env, want) || slices.Contains(f.cmds[0].Args, "--") {
		t.Errorf("command %+v, want %s and no forwarder", f.cmds[0], want)
	}
}
