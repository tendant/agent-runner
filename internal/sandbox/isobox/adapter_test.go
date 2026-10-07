package isobox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/conformance"
)

func roots(t *testing.T) map[string]string {
	d := t.TempDir()
	m := map[string]string{sandbox.RootWorkspace: d + "/ws", sandbox.RootHome: d + "/home", sandbox.RootTmp: d + "/tmp"}
	for _, p := range m {
		os.MkdirAll(p, 0o755)
	}
	return m
}

func spec(egress sandbox.EgressMode) sandbox.SandboxSpec {
	return sandbox.SandboxSpec{Version: 1,
		Filesystem: sandbox.FilesystemPolicy{
			Grants: []sandbox.PathGrant{{Path: sandbox.RootWorkspace, Access: sandbox.ReadWrite}, {Path: sandbox.RootHome, Access: sandbox.ReadOnly}},
			Deny:   []string{sandbox.RootHome + "/.ssh"}},
		Network:   sandbox.NetworkPolicy{Egress: egress},
		Resources: sandbox.ResourcePolicy{MemoryBytes: 1 << 30, PIDs: 64},
	}
}

func TestFlags(t *testing.T) {
	r := roots(t)
	b := New(Config{Roots: r, Backend: "gvisor"})
	f, _, err := b.flags(spec(sandbox.EgressNone))
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(f, " ")
	for _, want := range []string{"--profile=none", "--net=disable", "--write=scope", "--writable " + r[sandbox.RootWorkspace],
		"--read-deny " + r[sandbox.RootHome] + "/.ssh", "--memory 1073741824", "--pids 64", "--backend gvisor"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
	if strings.Contains(j, "--allow-temp") || strings.Contains(j, "--net=enable") {
		t.Errorf("unexpected flag: %s", j)
	}
	// restricted fails safe to no network.
	f, notes, _ := b.flags(spec(sandbox.EgressRestricted))
	if !strings.Contains(strings.Join(f, " "), "--net=disable") || len(notes) == 0 {
		t.Errorf("restricted: %v %v", f, notes)
	}
	// unmapped grant fails.
	s := spec(sandbox.EgressNone)
	s.Filesystem.Grants[0].Path = "/etc"
	if _, _, err := b.flags(s); err == nil {
		t.Error("unmapped path must fail")
	}
}

func TestParsePlan(t *testing.T) {
	be, en, cv := parsePlan("backend:  seatbelt\nenforces: env.scrub, net.disable\ncaveats:\n  - one\n  - two\nprofile:\n  x\n")
	if be != "seatbelt" || len(en) != 2 || len(cv) != 2 {
		t.Fatalf("%q %v %v", be, en, cv)
	}
}

// fakeIsobox emulates `isobox --print` and `isobox ... -- cmd`.
func fakeIsobox(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "isobox")
	script := `#!/bin/sh
print=0
for a in "$@"; do [ "$a" = "--print" ] && print=1; done
if [ $print = 1 ]; then
  echo "backend:  seatbelt"
  echo "enforces: env.scrub, fs.read.deny, fs.write.scope, net.disable"
  echo "caveats:"
  echo "  - test caveat"
  exit 0
fi
while [ "$1" != "--" ]; do shift; done; shift
exec "$@"
`
	os.WriteFile(p, []byte(script), 0o755)
	return p
}

func TestCheckGaps(t *testing.T) {
	b := New(Config{Binary: fakeIsobox(t), Roots: roots(t)})
	res, err := b.Check(context.Background(), spec(sandbox.EgressNone))
	if err != nil {
		t.Fatal(err)
	}
	gaps := map[sandbox.CapabilityID]bool{}
	for _, g := range res.Gaps {
		gaps[g.Capability] = true
	}
	if !gaps[sandbox.CapResourceMemory] || !gaps[sandbox.CapResourcePIDs] {
		t.Errorf("seatbelt-like plan must gap memory/pids: %+v", res.Gaps)
	}
	if gaps[sandbox.CapNetworkDirectNone] || gaps[sandbox.CapFSDeny] {
		t.Errorf("unexpected gaps: %+v", res.Gaps)
	}
	if err := sandbox.Admit(sandbox.ModeStrict, res); err == nil {
		t.Error("strict must reject")
	}
	// restricted egress is always a gap.
	res, _ = b.Check(context.Background(), spec(sandbox.EgressRestricted))
	found := false
	for _, g := range res.Gaps {
		found = found || g.Capability == sandbox.CapNetworkRestrictedEgress
	}
	if !found {
		t.Error("restricted egress must be a gap")
	}
}

func TestCheckEvidenceGating(t *testing.T) {
	ev := &conformance.Report{Results: map[string]conformance.Result{"fs.ssh_unreadable": {Status: conformance.Pass}}}
	b := New(Config{Binary: fakeIsobox(t), Roots: roots(t), Evidence: ev})
	res, _ := b.Check(context.Background(), spec(sandbox.EgressNone))
	for _, c := range res.Enforced {
		if c == sandbox.CapFSDeny {
			t.Error("fs.deny needs fs.symlink_escape too; must not be claimed")
		}
	}
	if _, err := New(Config{Binary: fakeIsobox(t), Roots: roots(t), RequireEvidence: true}).Check(context.Background(), spec(sandbox.EgressNone)); err == nil {
		t.Error("RequireEvidence without evidence must fail")
	}
}

func TestDenyInsideWritableIsGap(t *testing.T) {
	b := New(Config{Binary: fakeIsobox(t), Roots: roots(t)})
	s := spec(sandbox.EgressNone)
	s.Filesystem.Deny = []string{sandbox.RootWorkspace + "/.git"}
	res, _ := b.Check(context.Background(), s)
	for _, c := range res.Enforced {
		if c == sandbox.CapFSDeny {
			t.Error("deny inside writable grant cannot be enforced for writes")
		}
	}
}

func TestRunEnvStdinAndTimeout(t *testing.T) {
	r := roots(t)
	b := New(Config{Binary: fakeIsobox(t), Roots: r})
	os.Setenv("LEAK_ME", "1")
	defer os.Unsetenv("LEAK_ME")
	ps, _ := b.Prepare(context.Background(), spec(sandbox.EgressNone))
	var out bytes.Buffer
	code, err := ps.Run(context.Background(), sandbox.ProcSpec{Args: []string{"env"}, Dir: sandbox.RootWorkspace, Env: []string{"A=1"}, Stdout: &out})
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if strings.Contains(out.String(), "LEAK_ME") || !strings.Contains(out.String(), "A=1") {
		t.Errorf("env: %s", out.String())
	}
	code, _ = ps.Run(context.Background(), sandbox.ProcSpec{Args: []string{"sh", "-c", "exit 7"}, Dir: sandbox.RootWorkspace})
	if code != 7 {
		t.Errorf("exit code %d", code)
	}
	// Timeout: TERM then KILL, bounded.
	s := spec(sandbox.EgressNone)
	s.Resources.TimeoutSec, s.Resources.GraceSec = 1, 1
	ps2, _ := b.Prepare(context.Background(), s)
	start := time.Now()
	_, err = ps2.Run(context.Background(), sandbox.ProcSpec{Args: []string{"sleep", "30"}, Dir: sandbox.RootWorkspace})
	if err == nil || time.Since(start) > 6*time.Second {
		t.Errorf("timeout: %v after %v", err, time.Since(start))
	}
	ps2.Destroy(context.Background())
	if _, err := ps2.Run(context.Background(), sandbox.ProcSpec{Args: []string{"true"}}); err == nil {
		t.Error("run after destroy must fail")
	}
}

// Real isobox: offline plan from any host; full conformance only where the
// native backend exists (macOS Seatbelt / Linux runsc).
func TestRealIsobox(t *testing.T) {
	bin := os.Getenv("ISOBOX_BIN")
	if bin == "" {
		t.Skip("set ISOBOX_BIN to a built isobox")
	}
	be := os.Getenv("ISOBOX_BACKEND")
	b := New(Config{Binary: bin, Backend: be, Roots: roots(t)})
	res, err := b.Check(context.Background(), spec(sandbox.EgressNone))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s enforced=%v gaps=%v", res.Backend, res.Enforced, res.Gaps)
	if os.Getenv("ISOBOX_CONFORMANCE") == "" {
		return
	}
	if _, err := exec.LookPath(map[string]string{"seatbelt": "sandbox-exec", "gvisor": "runsc"}[be]); err != nil {
		t.Skip("backend runtime missing")
	}
	rep, err := conformance.Run(context.Background(), "isobox:"+be, func(e conformance.Env) sandbox.Backend {
		return New(Config{Binary: bin, Backend: be, Roots: e.Roots})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", rep.JSON())
	os.WriteFile(os.Getenv("ISOBOX_CONFORMANCE"), rep.JSON(), 0o644)
}

func TestCommandHonoursSpecTimeout(t *testing.T) {
	b := New(Config{Binary: fakeIsobox(t), Roots: roots(t)})
	s := spec(sandbox.EgressNone)
	s.Resources.TimeoutSec, s.Resources.GraceSec = 1, 1
	ps, _ := b.Prepare(context.Background(), s)
	defer ps.Destroy(context.Background())
	cmd, err := ps.(sandbox.Commander).Command(context.Background(), sandbox.ProcSpec{Args: []string{"sleep", "30"}, Dir: sandbox.RootWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cmd.Start()
	cmd.Wait()
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("Command ignored spec timeout: %v", d)
	}
}

func TestRunnerReadDenyIsAlwaysApplied(t *testing.T) {
	b := New(Config{Roots: roots(t), ReadDeny: []string{"/data/.env", "/data/state"}})
	f, _, err := b.flags(spec(sandbox.EgressOutbound))
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(f, " ")
	for _, want := range []string{"--read-deny /data/.env", "--read-deny /data/state"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
}
