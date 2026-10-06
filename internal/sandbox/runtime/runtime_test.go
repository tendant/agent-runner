package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
