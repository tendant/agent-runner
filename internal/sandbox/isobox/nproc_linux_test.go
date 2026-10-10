package isobox

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The test binary imports this package, so it is a shim too.
func TestNprocShimSetsTheLimitAndExecs(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(self, nprocShim, "50", "--", "sh", "-c", `grep "Max processes" /proc/self/limits`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if f := strings.Fields(string(out)); len(f) < 4 || f[2] != "50" || f[3] != "50" {
		t.Errorf("limits: %q", out)
	}
	if out, err := exec.Command(self, nprocShim, "50", "--", "no-such-command-x").CombinedOutput(); err == nil || !strings.Contains(string(out), "nproc shim") {
		t.Errorf("a missing command must fail: %v %s", err, out)
	}
}
