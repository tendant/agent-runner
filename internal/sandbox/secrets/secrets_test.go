package secrets

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

func TestDeliveryMethodsAndDowngrade(t *testing.T) {
	dir := t.TempDir()
	host := func(l string) (string, error) { return filepath.Join(dir, l), nil }
	p := MapProvider{"gh": "ghp_supersecretvalue", "k": "filesecretvalue"}
	d, err := Deliver([]sandbox.SecretGrant{
		{Name: "gh", Delivery: sandbox.DeliverEnv, Target: "GH_TOKEN"},
		{Name: "k", Delivery: sandbox.DeliverFile, Target: "/home/agent/k"},
	}, p, Options{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if d.Env[0] != "GH_TOKEN=ghp_supersecretvalue" {
		t.Errorf("env %v", d.Env)
	}
	fp := filepath.Join(dir, "/home/agent/k")
	if st, err := os.Stat(fp); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("file %v %v", st, err)
	}
	d.Cleanup()
	if _, err := os.Stat(fp); err == nil {
		t.Error("cleanup must remove files")
	}

	// FD requested but unsupported: downgraded to file and reported.
	d, err = Deliver([]sandbox.SecretGrant{{Name: "k", Delivery: sandbox.DeliverFD, Target: "/home/agent/k2"}}, p, Options{Host: host})
	if err != nil || len(d.Downgrades) != 1 || !strings.Contains(d.Downgrades[0], "fd->file") {
		t.Fatalf("%v %v", d, err)
	}
	d.Cleanup()
	if _, err := Deliver([]sandbox.SecretGrant{{Name: "k", Delivery: sandbox.DeliverFD}}, p, Options{Host: host, NoDowngrade: true}); err == nil {
		t.Error("NoDowngrade must reject")
	}
}

func TestFDDelivery(t *testing.T) {
	dir := t.TempDir()
	d, err := Deliver([]sandbox.SecretGrant{{Name: "k", Delivery: sandbox.DeliverFD, Target: "K_FD"}},
		MapProvider{"k": "fdsecretvalue"}, Options{FileDir: dir, Supported: []sandbox.SecretDelivery{sandbox.DeliverFD}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	b, _ := io.ReadAll(d.ExtraFiles[0])
	if string(b) != "fdsecretvalue" || d.Env[0] != "K_FD=3" {
		t.Errorf("%q %v", b, d.Env)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".secret-*")); len(left) != 0 {
		t.Errorf("temp file not unlinked: %v", left)
	}
}

func TestFailClosedAndRedaction(t *testing.T) {
	if _, err := Deliver([]sandbox.SecretGrant{{Name: "missing", Delivery: sandbox.DeliverEnv, Target: "X"}}, MapProvider{}, Options{}); err == nil {
		t.Error("missing secret must fail")
	}
	d, _ := Deliver([]sandbox.SecretGrant{{Name: "gh", Delivery: sandbox.DeliverEnv, Target: "GH"}}, MapProvider{"gh": "ghp_supersecretvalue"}, Options{})
	if got := d.Redact("push with ghp_supersecretvalue failed"); strings.Contains(got, "ghp_") || !strings.Contains(got, "[REDACTED:gh]") {
		t.Errorf("redact: %s", got)
	}
	if n := EnvNames(d.Env); len(n) != 1 || n[0] != "GH" {
		t.Errorf("%v", n)
	}
}

func TestEnvProviderOnlyReadsPrefixed(t *testing.T) {
	env := map[string]string{"SANDBOX_SECRET_GH_TOKEN": "v", "ANTHROPIC_API_KEY": "model", "SSH_AUTH_SOCK": "/s"}
	p := EnvProvider{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	if v, ok := p.Get("gh-token"); !ok || v != "v" {
		t.Error("prefixed lookup")
	}
	for _, n := range []string{"ANTHROPIC_API_KEY", "SSH_AUTH_SOCK", "anthropic_api_key"} {
		if _, ok := p.Get(n); ok {
			t.Errorf("policy must not reach %s", n)
		}
	}
}
