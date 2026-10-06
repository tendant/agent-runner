// Package e2e exercises the sandbox end to end with the real runtime, adapter,
// launcher, leases, supervisor and workspace export, using a fake `isobox`
// executable (so it runs anywhere). Real Seatbelt/gVisor enforcement is
// covered separately by cmd/sandbox-conformance on those hosts.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/lease"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
	"github.com/agent-runner/agent-runner/internal/sandbox/supervisor"
	"github.com/agent-runner/agent-runner/internal/sandbox/workspace"
)

const fakeIsobox = `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "--print" ]; then
    echo "backend:  seatbelt"
    echo "enforces: env.scrub, fs.read.deny, fs.write.scope, net.outbound, net.disable"
    exit 0
  fi
done
while [ "$1" != "--" ]; do shift; done; shift
exec "$@"
`

type harness struct {
	t      *testing.T
	state  string
	bin    string
	events []string
	mu     sync.Mutex
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, state: t.TempDir(), bin: filepath.Join(t.TempDir(), "isobox")}
	if err := os.WriteFile(h.bin, []byte(fakeIsobox), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) runtime(mode sandbox.Mode, policy string) *sandboxrt.Runtime {
	cfg := sandboxrt.Config{Mode: mode, StateDir: h.state, Instance: "e2e", IsoboxBin: h.bin, LeaseTTL: time.Second,
		OnEvent: func(e sandboxrt.Event) { h.mu.Lock(); h.events = append(h.events, e.Kind); h.mu.Unlock() }}
	if policy != "" {
		p := filepath.Join(h.t.TempDir(), "policy.json")
		os.WriteFile(p, []byte(policy), 0o600)
		cfg.PolicyFile = p
	}
	rt, err := sandboxrt.New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return rt
}

func (h *harness) has(kind string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.events {
		if e == kind {
			return true
		}
	}
	return false
}

func (h *harness) leaseCount() int {
	st, _ := lease.NewStore(filepath.Join(h.state, "leases"))
	l, _ := st.List()
	return len(l)
}

// basePolicy is the default system policy as JSON plus extra resource limits.
func basePolicy(resources string) string {
	return `{"version":1,
 "filesystem":{"grants":[{"path":"/workspace","access":"rw"},{"path":"/home/agent","access":"rw"},{"path":"/tmp","access":"rw"}]},
 "network":{"egress":"outbound"},
 "resources":` + resources + `}`
}

func runCLI(t *testing.T, ctx context.Context, ws string, name string, args ...string) (string, error) {
	l, ok := executor.LauncherFrom(ctx)
	if !ok {
		t.Fatal("run context carries no sandbox launcher")
	}
	cmd, release, err := l.Command(ctx, executor.LaunchSpec{Name: name, Args: args, Dir: ws})
	if err != nil {
		return "", err
	}
	defer release()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}

func TestHappyPathStrict(t *testing.T) {
	h := newHarness(t)
	rt := h.runtime(sandbox.ModeStrict, "")
	ws := t.TempDir()
	os.Setenv("HOST_SECRET_TOKEN", "leak-me")
	defer os.Unsetenv("HOST_SECRET_TOKEN")
	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{ThreadID: "th", RunID: "run1", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, run.Ctx, ws, "sh", "-c", "echo hello; echo HOME=$HOME; env")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "HOME="+filepath.Join(h.state, "home", "th")) {
		t.Errorf("output: %s", out)
	}
	if strings.Contains(out, "leak-me") {
		t.Error("host environment leaked into the sandbox")
	}
	if h.leaseCount() != 2 {
		t.Errorf("leases during run: %d", h.leaseCount())
	}
	run.Finish()
	if h.leaseCount() != 0 || !h.has("sandbox.started") || !h.has("sandbox.finished") {
		t.Errorf("leases=%d events=%v", h.leaseCount(), h.events)
	}
}

func TestPolicyRejection(t *testing.T) {
	h := newHarness(t)
	// The fake (Seatbelt-like) backend cannot cap memory.
	rt := h.runtime(sandbox.ModeStrict, basePolicy(`{"memory_bytes":1073741824}`))
	_, err := rt.Begin(context.Background(), sandboxrt.BeginReq{RunID: "run2", Workspace: t.TempDir()})
	var rej *sandboxrt.RejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want rejection, got %v", err)
	}
	var ge *sandbox.GapError
	if !errors.As(err, &ge) || ge.Gaps[0].Capability != sandbox.CapResourceMemory {
		t.Errorf("want memory gap, got %v", err)
	}
	if h.leaseCount() != 0 || !h.has("sandbox.rejected") {
		t.Errorf("rejected run must hold no leases: %d %v", h.leaseCount(), h.events)
	}
	// Permissive runs the same policy and reports the gap.
	h2 := newHarness(t)
	run, err := h2.runtime(sandbox.ModePermissive, basePolicy(`{"memory_bytes":1073741824}`)).
		Begin(context.Background(), sandboxrt.BeginReq{RunID: "run2b", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run.Finish()
}

func TestTimeoutKillsProcessTree(t *testing.T) {
	h := newHarness(t)
	rt := h.runtime(sandbox.ModeStrict, basePolicy(`{"timeout_sec":1,"grace_sec":1}`))
	ws := t.TempDir()
	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{RunID: "run3", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Finish()
	start := time.Now()
	_, err = runCLI(t, run.Ctx, ws, "sh", "-c", "sleep 30; echo FINISHED")
	if err == nil || time.Since(start) > 10*time.Second {
		t.Errorf("run must be killed by the timeout: err=%v after %v", err, time.Since(start))
	}
}

func TestCrashRecoveryReapsOrphanedSandbox(t *testing.T) {
	h := newHarness(t)
	rt := h.runtime(sandbox.ModeStrict, "")
	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{ThreadID: "th", RunID: "run4", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// A sandbox process the (about to "crash") runner left behind, findable by tag.
	orphan := exec.Command("sleep", "61", "sbx.run4")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-orphan.Process.Pid, syscall.SIGKILL); orphan.Wait() }()

	// The runner dies: its owner process is gone and its lease stops renewing.
	dead := exec.Command("true")
	dead.Start()
	deadOwner := lease.Owner{InstanceID: "e2e", PID: dead.Process.Pid, Start: lease.ProcStart(dead.Process.Pid)}
	dead.Wait()
	st, _ := lease.NewStore(filepath.Join(h.state, "leases"))
	if err := st.Renew("sandbox:run4", lease.SelfOwner("e2e"), time.Hour, func(r *lease.Record) {
		r.Owner, r.Expires = deadOwner, time.Now().Add(-time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	_ = run // the dead runner never calls Finish

	sup := &supervisor.Supervisor{Store: st, Grace: 300 * time.Millisecond}
	var orphans []string
	sup.OnOrphan = func(r lease.Record) { orphans = append(orphans, r.RunID) }
	if n, _ := sup.Sweep(); n != 1 || len(orphans) != 1 || orphans[0] != "run4" {
		t.Fatalf("sweep acted=%d orphans=%v", n, orphans)
	}
	done := make(chan struct{})
	go func() { orphan.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("orphaned sandbox process survived the supervisor")
	}
	if l, _ := st.List(); len(l) != 1 || l[0].Kind != "thread" {
		t.Errorf("only the (takeover-able) thread lease should remain: %+v", l)
	}
}

func TestExportConflictQuarantinesWork(t *testing.T) {
	d := t.TempDir()
	thread, runDir, quarantine := filepath.Join(d, "thread"), filepath.Join(d, "run"), filepath.Join(d, "q")
	os.MkdirAll(thread, 0o755)
	os.WriteFile(filepath.Join(thread, "main.go"), []byte("package main"), 0o644)
	base, err := workspace.Stage(thread, runDir)
	if err != nil {
		t.Fatal(err)
	}
	// The agent edits its copy while another actor edits the Thread workspace.
	os.WriteFile(filepath.Join(runDir, "main.go"), []byte("package main // agent"), 0o644)
	os.WriteFile(filepath.Join(thread, "main.go"), []byte("package main // human"), 0o644)

	v := executor.NewValidator(nil, false)
	validate := func(changed []string) error {
		if ve := v.ValidateDiff(changed, nil); ve != nil {
			return errors.New(ve.Code + ": " + ve.Message)
		}
		return nil
	}
	res, err := workspace.Export(thread, runDir, base, validate, quarantine, "run5")
	if err != nil || res.Outcome != workspace.Quarantined || res.Reason != "conflict" {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(thread, "main.go")); string(b) != "package main // human" {
		t.Error("Thread workspace must keep the other actor's edit")
	}
	if b, _ := os.ReadFile(filepath.Join(quarantine, "run5", "files", "main.go")); string(b) != "package main // agent" {
		t.Error("agent's work was lost")
	}

	// A diff touching .git is rejected by the existing validator and also kept.
	thread2, run2 := filepath.Join(d, "t2"), filepath.Join(d, "r2")
	os.MkdirAll(thread2, 0o755)
	os.WriteFile(filepath.Join(thread2, "a"), []byte("a"), 0o644)
	base2, _ := workspace.Stage(thread2, run2)
	os.MkdirAll(filepath.Join(run2, ".git", "hooks"), 0o755)
	os.WriteFile(filepath.Join(run2, ".git", "hooks", "pre-commit"), []byte("x"), 0o755)
	res, err = workspace.Export(thread2, run2, base2, validate, quarantine, "run6")
	if err != nil || res.Outcome != workspace.Quarantined || !strings.Contains(res.Reason, "GIT_DIR") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestMisconfiguredSandboxNeverRunsOnHost(t *testing.T) {
	rt := sandboxrt.Failed(errors.New("boom"))
	if _, err := rt.Begin(context.Background(), sandboxrt.BeginReq{RunID: "x", Workspace: t.TempDir()}); err == nil {
		t.Fatal("must reject")
	}
}
