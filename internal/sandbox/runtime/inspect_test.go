package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// inspectRun begins a run with inspection on and returns it, its resolved
// spec and the environment its first launch got.
func inspectRun(t *testing.T) (*Run, string) {
	t.Helper()
	got := &sandbox.ProcSpec{}
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Second,
		EgressInspect: true, InspectExcept: []string{"pinned.example.com"},
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{nil, got} }})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Finish)
	l, _ := executor.LauncherFrom(run.Ctx)
	_, release, err := l.Command(run.Ctx, executor.LaunchSpec{Name: "claude", Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	release()
	return run, strings.Join(got.Env, "\n")
}

// With inspection on, the run's spec says so (with its exceptions), its
// certificate and a bundle of the system roots with it are in its tmp, and
// the sandbox's tools are pointed at them.
func TestInspectionTrust(t *testing.T) {
	run, env := inspectRun(t)
	if !run.Spec.Network.EgressInspect || len(run.Spec.Network.EgressInspectExcept) != 1 {
		t.Fatalf("spec network %+v", run.Spec.Network)
	}
	tmp := run.roots[sandbox.RootTmp]
	ca, err := os.ReadFile(filepath.Join(tmp, "agentbox-egress-ca.pem"))
	if err != nil || !strings.Contains(string(ca), "BEGIN CERTIFICATE") {
		t.Fatalf("ca %v", err)
	}
	bundle, _ := os.ReadFile(filepath.Join(tmp, "agentbox-egress-roots.pem"))
	if !strings.HasSuffix(string(bundle), string(ca)) || strings.Count(string(bundle), "BEGIN CERTIFICATE") < 10 {
		t.Errorf("bundle: %d certificates, ends with the run's: %v", strings.Count(string(bundle), "BEGIN CERTIFICATE"),
			strings.HasSuffix(string(bundle), string(ca)))
	}
	for _, v := range []string{"SSL_CERT_FILE=" + filepath.Join(tmp, "agentbox-egress-roots.pem"), "GIT_SSL_CAINFO=",
		"REQUESTS_CA_BUNDLE=", "NODE_EXTRA_CA_CERTS=" + filepath.Join(tmp, "agentbox-egress-ca.pem")} {
		if !strings.Contains(env, v) {
			t.Errorf("env lacks %s", v)
		}
	}
}

// A host with no system root bundle cannot inspect: the run goes on with
// hosts only, and its spec says it was not inspected.
func TestInspectionWithoutSystemRoots(t *testing.T) {
	saved := systemRoots
	systemRoots = []string{filepath.Join(t.TempDir(), "none.pem")}
	t.Cleanup(func() { systemRoots = saved })
	run, env := inspectRun(t)
	if run.Spec.Network.EgressInspect || strings.Contains(env, "SSL_CERT_FILE=") {
		t.Errorf("inspected without roots: %+v", run.Spec.Network)
	}
}
