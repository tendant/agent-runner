package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
)

type fakeBackend struct {
	gaps []sandbox.Gap
	got  *sandbox.ProcSpec
}

func (f fakeBackend) Check(context.Context, sandbox.SandboxSpec) (sandbox.CheckResult, error) {
	return sandbox.CheckResult{Backend: "fake", Gaps: f.gaps}, nil
}
func (f fakeBackend) Prepare(context.Context, sandbox.SandboxSpec) (sandbox.PreparedSandbox, error) {
	return fakePrepared{f}, nil
}

type fakePrepared struct{ f fakeBackend }

func (fakePrepared) Run(context.Context, sandbox.ProcSpec) (int, error) { return 0, nil }
func (fakePrepared) Destroy(context.Context) error                      { return nil }
func (p fakePrepared) Command(ctx context.Context, ps sandbox.ProcSpec) (*exec.Cmd, error) {
	*p.f.got = ps
	return exec.CommandContext(ctx, "true"), nil
}

type evs struct {
	mu sync.Mutex
	k  []string
}

func (e *evs) add(ev Event) { e.mu.Lock(); e.k = append(e.k, ev.Kind); e.mu.Unlock() }
func (e *evs) has(k string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, x := range e.k {
		if x == k {
			return true
		}
	}
	return false
}

func newRT(t *testing.T, mode sandbox.Mode, gaps []sandbox.Gap) (*Runtime, *evs, *sandbox.ProcSpec) {
	e := &evs{}
	got := &sandbox.ProcSpec{}
	rt, err := New(Config{Mode: mode, StateDir: t.TempDir(), Instance: "test", OnEvent: e.add, LeaseTTL: time.Second,
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{gaps, got} }})
	if err != nil {
		t.Fatal(err)
	}
	return rt, e, got
}

func leases(t *testing.T, rt *Runtime) int {
	l, _ := rt.store.List()
	return len(l)
}

func TestOffIsPassthrough(t *testing.T) {
	rt, _ := New(Config{Mode: sandbox.ModeOff})
	ctx := context.Background()
	run, err := rt.Begin(ctx, BeginReq{})
	if err != nil || run.Ctx != ctx {
		t.Fatal(err)
	}
	if _, ok := executor.LauncherFrom(run.Ctx); ok {
		t.Error("off must not install a sandbox launcher")
	}
	run.Finish()
}

func TestStrictRejectsBeforeAnyLease(t *testing.T) {
	rt, e, _ := newRT(t, sandbox.ModeStrict, []sandbox.Gap{{Capability: sandbox.CapResourceMemory}})
	_, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	var rej *RejectedError
	if err == nil || !asRejected(err, &rej) {
		t.Fatalf("want RejectedError, got %v", err)
	}
	if !e.has("sandbox.rejected") || leases(t, rt) != 0 {
		t.Errorf("events=%v leases=%d", e.k, leases(t, rt))
	}
}

func asRejected(err error, out **RejectedError) bool {
	r, ok := err.(*RejectedError)
	*out = r
	return ok
}

func TestPermissiveRunsAndReportsGaps(t *testing.T) {
	rt, e, got := newRT(t, sandbox.ModePermissive, []sandbox.Gap{{Capability: sandbox.CapResourceMemory}})
	ws := t.TempDir()
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r2", ThreadID: "t2", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if leases(t, rt) != 2 { // thread + sandbox
		t.Errorf("leases=%d", leases(t, rt))
	}
	l, ok := executor.LauncherFrom(run.Ctx)
	if !ok {
		t.Fatal("run ctx must carry the sandbox launcher")
	}
	cmd, release, err := l.Command(run.Ctx, executor.LaunchSpec{Name: "claude", Args: []string{"--print"}, Dir: ws, ExtraEnv: []string{"X=1"}})
	if err != nil || cmd == nil {
		t.Fatal(err)
	}
	release()
	env := strings.Join(got.Env, "\n")
	home := filepath.Join(rt.cfg.StateDir, "home", "t2")
	if !strings.Contains(env, "HOME="+home) || !strings.Contains(env, "X=1") || got.Tag != "r2" || got.Args[0] != "claude" {
		t.Errorf("proc spec: %+v", got)
	}
	if !strings.Contains(env, "CLAUDE_CODE_TMPDIR="+filepath.Join(rt.cfg.StateDir, "runs", "r2", "tmp")) {
		t.Errorf("claude's scratch dir must be the run's tmp: %s", env)
	}
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude")) {
		t.Errorf("claude's config dir must be in the sandbox home: %s", env)
	}
	run.Finish()
	run.Finish() // idempotent
	if leases(t, rt) != 0 {
		t.Errorf("leases not released: %d", leases(t, rt))
	}
	if _, err := os.Stat(filepath.Join(rt.cfg.StateDir, "runs", "r2")); err == nil {
		t.Error("run dir must be removed")
	}
	if !e.has("sandbox.started") || !e.has("sandbox.finished") {
		t.Errorf("events: %v", e.k)
	}
}

func TestWideningLayerRejected(t *testing.T) {
	rt, e, _ := newRT(t, sandbox.ModePermissive, nil)
	_, err := rt.Begin(context.Background(), BeginReq{RunID: "r3", Workspace: t.TempDir(), Layers: []sandbox.Layer{
		{Name: sandbox.LayerRun, Spec: sandbox.SandboxSpec{Version: 1, Network: sandbox.NetworkPolicy{Egress: sandbox.EgressOutbound, EgressAllow: nil},
			Resources: sandbox.ResourcePolicy{}}},
		{Name: "bad", Spec: sandbox.SandboxSpec{Version: 1, Filesystem: sandbox.FilesystemPolicy{Grants: []sandbox.PathGrant{{Path: "/etc", Access: sandbox.ReadWrite}}}}},
	}})
	if err == nil || !e.has("sandbox.rejected") {
		t.Errorf("err=%v events=%v", err, e.k)
	}
}

func TestNewRejectsBadMode(t *testing.T) {
	if _, err := New(Config{Mode: "yolo", StateDir: t.TempDir()}); err == nil {
		t.Error("bad mode must fail")
	}
}

func TestFailedRuntimeRejectsEverything(t *testing.T) {
	rt := Failed(os.ErrInvalid)
	if rt.Mode() != sandbox.ModeStrict {
		t.Fatal("failed runtime must be strict")
	}
	if _, err := rt.Begin(context.Background(), BeginReq{RunID: "x", Workspace: t.TempDir()}); err == nil {
		t.Error("must reject")
	}
}

func TestReadDenyKeepsOnlyTheRunsOwnDirectories(t *testing.T) {
	data := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(data, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ws := mk("tmp/session-1/workspace")
	mk("tmp/session-2/workspace")
	home := mk("state/sandbox/home/thread-1")
	mk("state/sandbox/home/thread-2")
	tmp := mk("state/sandbox/runs/run-1/tmp")
	mk("state/sandbox/leases")
	mk("state/sessions")
	mk("logs")
	os.WriteFile(filepath.Join(data, ".env.local"), []byte("TOKEN=x"), 0o600)

	got := readDeny([]string{
		filepath.Join(data, "state"), filepath.Join(data, "tmp"), filepath.Join(data, "logs"),
		filepath.Join(data, ".env.local"), filepath.Join(data, "missing"),
	}, []string{ws, home, tmp})

	real, _ := filepath.EvalSymlinks(data) // macOS: /var -> /private/var
	rel := map[string]bool{}
	for _, p := range got {
		for _, base := range []string{data, real} {
			if r, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(r, "..") {
				rel[r] = true
			}
		}
	}
	for _, want := range []string{".env.local", "logs", "state/sessions", "state/sandbox/leases",
		"state/sandbox/home/thread-2", "tmp/session-2"} {
		if !rel[want] {
			t.Errorf("not denied: %s (got %v)", want, got)
		}
	}
	for _, kept := range []string{"state", "tmp", "tmp/session-1", "tmp/session-1/workspace",
		"state/sandbox/home/thread-1", "state/sandbox/runs/run-1/tmp", "missing"} {
		if rel[kept] {
			t.Errorf("denied %s, which the run needs (or does not exist)", kept)
		}
	}
}

func TestPolicyInheritsDefaultsAndAcceptsInlineJSON(t *testing.T) {
	file := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(file, []byte(`{"version":1,"network":{"egress":"none"}}`), 0o600)
	for _, value := range []string{file, ` {"version":1,"network":{"egress":"none"}}`} {
		spec, err := loadPolicy(value)
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if spec.Network.Egress != sandbox.EgressNone {
			t.Errorf("%q: egress = %q, want none", value, spec.Network.Egress)
		}
		// Unset in the policy: the default grants still make the workspace writable.
		if len(spec.Filesystem.Grants) != len(DefaultSystemSpec().Filesystem.Grants) {
			t.Errorf("%q: grants = %v, want the default grants", value, spec.Filesystem.Grants)
		}
	}
	// A set field replaces the default's.
	spec, err := loadPolicy(`{"version":1,"filesystem":{"grants":[{"path":"/workspace","access":"rw"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Filesystem.Grants) != 1 || spec.Network.Egress != sandbox.EgressOutbound {
		t.Errorf("spec = %+v", spec)
	}
	if _, err := loadPolicy(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing policy file must fail")
	}
	if _, err := loadPolicy(`{"version":1,"bogus":true}`); err == nil {
		t.Error("an unknown field must fail")
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"4g": 4 << 30, "4G": 4 << 30, "4GiB": 4 << 30, "4gb": 4 << 30, " 512m ": 512 << 20,
		"64k": 64 << 10, "1t": 1 << 40, "1048576": 1 << 20, "100b": 100,
	} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "g", "0", "-1g", "1.5g", "4x", "lots", "99999999999t"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) must fail", in)
		}
	}
}

func TestMemoryCapsEveryRun(t *testing.T) {
	newWith := func(cfg Config) (*Runtime, error) {
		cfg.Mode, cfg.StateDir = sandbox.ModePermissive, t.TempDir()
		return New(cfg)
	}
	rt, err := newWith(Config{Memory: "4g"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.system.Resources.MemoryBytes; got != 4<<30 {
		t.Errorf("memory = %d, want 4 GiB", got)
	}
	// It overrides the policy's memory_bytes; the policy's other fields stay.
	rt, err = newWith(Config{Memory: "512m", PolicyFile: `{"version":1,"network":{"egress":"none"},"resources":{"memory_bytes":1073741824}}`})
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.system.Resources.MemoryBytes; got != 512<<20 || rt.system.Network.Egress != sandbox.EgressNone {
		t.Errorf("spec = %+v", rt.system)
	}
	if _, err := newWith(Config{Memory: "4 gigs"}); err == nil {
		t.Error("a bad memory size must fail")
	}
	if rp, err := newWith(Config{PIDs: "1024", PolicyFile: `{"version":1,"resources":{"pids":64}}`}); err != nil || rp.system.Resources.PIDs != 1024 {
		t.Errorf("pids: %v %+v", err, rp)
	}
	for _, bad := range []string{"0", "-5", "many", "1k"} {
		if _, err := newWith(Config{PIDs: bad}); err == nil {
			t.Errorf("pids %q must fail", bad)
		}
	}
	rc, err := newWith(Config{CPUs: "1.5", PolicyFile: `{"version":1,"resources":{"cpus":8}}`})
	if err != nil || rc.system.Resources.CPUs != 1.5 {
		t.Errorf("cpus: %v %+v", err, rc)
	} else if caps := sandbox.Required(rc.system); !containsCap(caps, sandbox.CapResourceCPU) {
		t.Errorf("a CPU cap must require %s, got %v", sandbox.CapResourceCPU, caps)
	}
	for _, bad := range []string{"0", "-1", "two"} {
		if _, err := newWith(Config{CPUs: bad}); err == nil {
			t.Errorf("cpus %q must fail", bad)
		}
	}
	// Seatbelt cannot cap memory: a capped run reports resource.memory as a gap there.
	if caps := sandbox.Required(rt.system); !containsCap(caps, sandbox.CapResourceMemory) {
		t.Errorf("a memory cap must require %s, got %v", sandbox.CapResourceMemory, caps)
	}
}

func containsCap(caps []sandbox.CapabilityID, c sandbox.CapabilityID) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// "outbound" policy is restricted egress through the gateway:
// any public host by default, or the configured hosts.
func TestEgressMapping(t *testing.T) {
	for _, tc := range []struct {
		allow []string
		want  []string
	}{{nil, []string{"*"}}, {[]string{"github.com", "*.npmjs.org"}, []string{"github.com", "*.npmjs.org"}}} {
		r, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", EgressAllow: tc.allow})
		if err != nil {
			t.Fatal(err)
		}
		n := r.system.Network
		if n.Egress != sandbox.EgressRestricted || !slices.Equal(n.EgressAllow, tc.want) {
			t.Errorf("allow %v: egress %s %v, want restricted %v", tc.allow, n.Egress, n.EgressAllow, tc.want)
		}
	}
	if _, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", EgressAllow: []string{"*.*.com"}}); err == nil {
		t.Error("a bad egress pattern must fail closed")
	}
	_, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", EgressAllow: []string{"@github", "@nope"}})
	if err == nil || !strings.Contains(err.Error(), "@nope") || !strings.Contains(err.Error(), "@github") {
		t.Errorf("an unknown set must fail closed, naming the known ones: %v", err)
	}
}

// A disk cap is the runtime's to enforce: a backend that reports
// resource.disk as a gap does not keep a strict run from starting, and a run
// whose files pass the cap is ended, with the reason as its context's cause.
func TestDiskCapEndsTheRun(t *testing.T) {
	e := &evs{}
	got := &sandbox.ProcSpec{}
	gaps := []sandbox.Gap{{Capability: sandbox.CapResourceDisk, Reason: "not enforced by backend"}}
	rt, err := New(Config{Mode: sandbox.ModeStrict, StateDir: t.TempDir(), Instance: "test", OnEvent: e.add, LeaseTTL: time.Second,
		Disk: "1m", DiskPoll: 10 * time.Millisecond,
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{gaps, got} }})
	if err != nil {
		t.Fatal(err)
	}
	if rt.system.Resources.DiskBytes != 1<<20 {
		t.Fatalf("disk = %d", rt.system.Resources.DiskBytes)
	}
	ws := t.TempDir()
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", Workspace: ws})
	if err != nil {
		t.Fatalf("a strict run with a disk cap must start: %v", err)
	}
	defer run.Finish()
	select {
	case <-run.Ctx.Done():
		t.Fatal("run ended under its cap")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(ws, "big"), bytes.Repeat([]byte("x"), 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("run not ended past its disk cap")
	}
	var le *LimitError
	if !errors.As(context.Cause(run.Ctx), &le) || le.Resource != "disk" || le.Used < 2<<20 {
		t.Fatalf("cause = %v", context.Cause(run.Ctx))
	}
	if !strings.Contains(le.Error(), "1.0 MiB") || !e.has("sandbox.disk_limit") {
		t.Errorf("error %q, events %v", le.Error(), e.k)
	}
}

func TestDiskCapConfig(t *testing.T) {
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Disk: "20g", PolicyFile: `{"version":1,"resources":{"disk_bytes":1024}}`})
	if err != nil || rt.system.Resources.DiskBytes != 20<<30 {
		t.Fatalf("disk: %v %+v", err, rt)
	}
	if _, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Disk: "lots"}); err == nil {
		t.Error("a bad disk size must fail")
	}
	// Without a cap, the check is the backend's as it was.
	res := sandbox.CheckResult{Gaps: []sandbox.Gap{{Capability: sandbox.CapResourceDisk}}}
	if out := withDiskWatch(res, sandbox.SandboxSpec{}); len(out.Gaps) != 1 {
		t.Errorf("no cap: %+v", out)
	}
	out := withDiskWatch(res, sandbox.SandboxSpec{Resources: sandbox.ResourcePolicy{DiskBytes: 1}})
	if len(out.Gaps) != 0 || !slices.Contains(out.Enforced, sandbox.CapResourceDisk) || len(out.Caveats) != 1 || len(res.Gaps) != 1 {
		t.Errorf("cap: %+v (input %+v)", out, res)
	}
}
