package isobox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

func TestGVisorUnreachableParent(t *testing.T) {
	if os.Getuid() == 0 || os.Getgid() == 0 {
		t.Skip("needs a non-root owner: root-owned dirs get their owner bits")
	}
	home := filepath.Join(openTempDir(t), "home")
	ws := filepath.Join(home, "data", "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := gvisorUnreachable(ws); err != nil {
		t.Fatalf("0755 parents: %v", err)
	}
	if err := os.Chmod(home, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(home, 0o755) })
	err := gvisorUnreachable(ws)
	if err == nil || !strings.Contains(err.Error(), "cannot enter "+home+" ") || !strings.Contains(err.Error(), "chmod o+x "+home) {
		t.Fatalf("0750 parent: got %v, want it named", err)
	}
	if err := os.Chmod(home, 0o751); err != nil { // search, no listing: enough
		t.Fatal(err)
	}
	if err := gvisorUnreachable(ws); err != nil {
		t.Fatalf("0751 parent: %v", err)
	}

	// Check rejects the run on gVisor only.
	os.Chmod(home, 0o750)
	roots := map[string]string{sandbox.RootWorkspace: ws, sandbox.RootHome: openTempDir(t), sandbox.RootTmp: openTempDir(t)}
	b := New(Config{Binary: "true", Backend: "gvisor", Roots: roots})
	if _, err := b.Check(context.Background(), sandbox.SandboxSpec{}); err == nil || !strings.Contains(err.Error(), "cannot enter "+home) {
		t.Fatalf("gvisor Check: got %v", err)
	}
	// Backend left unset: what isobox resolves it to decides.
	for plan, reject := range map[string]bool{"gvisor": true, "docker-ephemeral": false} {
		bin := filepath.Join(t.TempDir(), "isobox")
		os.WriteFile(bin, []byte("#!/bin/sh\necho 'backend:  "+plan+"'\n"), 0o755)
		_, err := New(Config{Binary: bin, Roots: roots}).Check(context.Background(), sandbox.SandboxSpec{})
		if got := err != nil && strings.Contains(err.Error(), "cannot enter"); got != reject {
			t.Errorf("unset backend resolved to %s: got %v", plan, err)
		}
	}
}
