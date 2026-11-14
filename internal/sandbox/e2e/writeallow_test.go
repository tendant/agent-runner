package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
)

// writeScript runs in the sandbox: it writes into the WriteAllow directory
// (a file and a SQLite-style journal beside an existing one), and tries to
// read the hidden credential beside them and a directory nobody granted.
const writeScript = `#!/bin/sh
d="$DATA_DIR_UNDER_TEST"
echo hello > "$d/new.txt" && echo "write:ok"
echo more >> "$d/existing.db" && echo "append:ok"
echo j > "$d/existing.db-journal" && echo "journal:ok"
if [ -s "$d/oauth-token.json" ] && grep -q secret "$d/oauth-token.json" 2>/dev/null; then echo "hidden:read"; else echo "hidden:blocked"; fi
if echo x > "$d/oauth-token.json" 2>/dev/null; then echo "hidden:written"; fi
if cat "$UNGRANTED/file" >/dev/null 2>&1; then echo "ungranted:read"; fi
`

// TestWriteAllowHide runs a script in agentbox's real backend with a
// WriteAllow directory holding a Hide file. Set E2E_REAL_SANDBOX=1 on a host
// with gVisor (Linux) or Seatbelt (macOS).
func TestWriteAllowHide(t *testing.T) {
	if os.Getenv("E2E_REAL_SANDBOX") == "" {
		t.Skip("set E2E_REAL_SANDBOX=1 to run against agentbox's real backend")
	}
	if goruntime.GOOS != "darwin" && goruntime.GOOS != "linux" {
		t.Skip("needs agentbox's real backend: Seatbelt or gVisor")
	}
	base := t.TempDir()
	data := filepath.Join(base, "mail")
	ungranted := filepath.Join(base, "other")
	for _, d := range []string{data, ungranted} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Join(data, "oauth-token.json")
	for p, body := range map[string]string{
		filepath.Join(data, "existing.db"): "db\n",
		token:                              `{"refresh_token":"secret"}`,
		filepath.Join(ungranted, "file"):   "x",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := sandboxrt.New(sandboxrt.Config{
		Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "e2e", LeaseTTL: 5 * time.Second,
		ScopedReads: true, WriteAllow: []string{data}, Hide: []string{token},
		OnEvent: func(e sandboxrt.Event) { t.Logf("event %s %s %s", e.Kind, e.Backend, e.Detail) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	script := filepath.Join(ws, "claude")
	if err := os.WriteFile(script, []byte(writeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{RunID: "e2e-write", ThreadID: "e2e-thread", Project: "e2e-project", Workspace: ws, MaxSecond: 120})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runWithEnv(t, run.Ctx, ws, script, []string{"DATA_DIR_UNDER_TEST=" + data, "UNGRANTED=" + ungranted})
	run.Finish()
	t.Logf("script output:\n%s", out)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	for _, want := range []string{"write:ok", "append:ok", "journal:ok", "hidden:blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, bad := range []string{"hidden:read", "hidden:written", "ungranted:read"} {
		if strings.Contains(out, bad) {
			t.Errorf("the sandbox did what it must not: %s", bad)
		}
	}
	// The writes reached the host; the token is untouched.
	if b, err := os.ReadFile(filepath.Join(data, "new.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("new.txt on the host: %q %v", b, err)
	}
	if b, _ := os.ReadFile(token); !strings.Contains(string(b), "secret") {
		t.Errorf("token changed: %q", b)
	}
}

func runWithEnv(t *testing.T, ctx context.Context, ws, name string, env []string) (string, error) {
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
