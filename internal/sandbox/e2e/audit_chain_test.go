//go:build e2e_audit

// The audit chain, end to end: a real sandbox (agentbox's real backend on
// this host) runs an agent that calls its model through the broker to a real
// ledger-gate and makes network connections, while the runtime writes the
// audit record agent-gate ingests. Driven by agent-gate's e2e/local.sh, which
// starts ledger-gate and checks the result:
//
//	E2E_LEDGER_URL   ledger-gate base URL
//	E2E_BROKER_KEY   a ledger-gate key marked asserts_run
//	E2E_AUDIT_DIR    where to write the audit record (small segments, so
//	                 the record spans several)
//	E2E_RUN_ID       the run's id (default e2e-run-a)
//	E2E_FORGED_RUN   a run id the agent tries to charge its calls to
package e2e

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
)

// agentScript is the "agent": what a compromised or careless one might do.
const agentScript = `#!/bin/sh
body='{"model":"mock","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
model() { label=$1; shift
  curl -s -o /dev/null -w "$label:%{http_code}\n" --max-time 20 --noproxy '*' \
  -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" -H "content-type: application/json" "$@" -d "$body" "$ANTHROPIC_BASE_URL/v1/messages"; }
model model1
model model2
# Charge a call to another run: the broker must replace this header.
model forged -H "Agentbox-Run-Id: $FORGED_RUN"
# Network: one granted host, one not, the cloud metadata address, and
# ledger-gate directly (around the broker), all through the egress gateway.
# --noproxy '': through the gateway even where NO_PROXY would skip it.
net() { curl -s -o /dev/null -w "$1:%{http_code}\n" --max-time 15 --noproxy '' --proxy "$HTTPS_PROXY" "$2"; }
net allowed https://example.com/
net denied https://example.org/
net metadata http://169.254.169.254/latest/meta-data/
net ledger-direct "$LEDGER_URL/v1/messages"
exit 0 # refused connections are the expected outcome, not the agent failing
`

func TestAuditChain(t *testing.T) {
	ledger, key, auditDir := os.Getenv("E2E_LEDGER_URL"), os.Getenv("E2E_BROKER_KEY"), os.Getenv("E2E_AUDIT_DIR")
	if ledger == "" || key == "" || auditDir == "" {
		t.Skip("run through agent-gate's e2e/local.sh")
	}
	// The agent script is launched by its host path: Seatbelt runs on the
	// host's filesystem, and gVisor mounts the workspace at its host path.
	if goruntime.GOOS != "darwin" && goruntime.GOOS != "linux" {
		t.Skip("needs agentbox's real backend: Seatbelt or gVisor")
	}
	runID := envOr("E2E_RUN_ID", "e2e-run-a")
	forged := envOr("E2E_FORGED_RUN", "e2e-run-forged")
	rec, err := audit.OpenDirRecorder(auditDir, "e2e-host", audit.DirOptions{Sync: true, SegmentBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	upstream, err := url.Parse(ledger)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := sandboxrt.New(sandboxrt.Config{
		Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "e2e", LeaseTTL: 5 * time.Second,
		EgressAllow: []string{"example.com"}, EgressPorts: []int{80, 443},
		Models: map[string]*sandboxrt.ModelChannel{
			sandboxrt.ProviderAnthropic: {Upstream: upstream, AuthHeader: "x-api-key", AuthValue: key, Paths: sandboxrt.AnthropicPaths, Ledger: true},
		},
		Audit:   rec,
		OnEvent: func(e sandboxrt.Event) { t.Logf("event %s %s %s", e.Kind, e.Backend, e.Detail) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	agent := filepath.Join(ws, "claude") // launched as "claude": it gets the model channel claude would
	if err := os.WriteFile(agent, []byte(agentScript), 0o755); err != nil {
		t.Fatal(err)
	}
	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{RunID: runID, ThreadID: "e2e-thread", Workspace: ws, MaxSecond: 300})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCLIEnv(t, run.Ctx, ws, agent, []string{"FORGED_RUN=" + forged, "LEDGER_URL=" + ledger})
	run.Finish()
	t.Logf("agent output:\n%s", out)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	for _, want := range []string{"model1:200", "model2:200", "forged:200", "allowed:200"} {
		if !strings.Contains(out, want) {
			t.Errorf("agent output lacks %q", want)
		}
	}
	for _, refused := range []string{"denied:200", "metadata:200", "ledger-direct:200"} {
		if strings.Contains(out, refused) {
			t.Errorf("the sandbox reached what it must not: %s", refused)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// runCLIEnv is runCLI with extra environment for the command.
func runCLIEnv(t *testing.T, ctx context.Context, ws, name string, env []string) (string, error) {
	l, ok := executor.LauncherFrom(ctx)
	if !ok {
		t.Fatal("run context carries no sandbox launcher")
	}
	cmd, release, err := l.Command(ctx, executor.LaunchSpec{Name: name, Dir: ws, ExtraEnv: env})
	if err != nil {
		return "", err
	}
	defer release()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}
