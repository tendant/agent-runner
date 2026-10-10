package isobox

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The test binary imports this package, so it is a shim too. The shimmed
// command must not fork: outside a sandbox RLIMIT_NPROC counts every process
// of the (non-root) user on the host, so a fork would fail with EAGAIN.
func TestNprocShimSetsTheLimitAndExecs(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(self, nprocShim, "50", "--", "cat", "/proc/self/limits").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var f []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Max processes") {
			f = strings.Fields(line)
		}
	}
	if len(f) < 4 || f[2] != "50" || f[3] != "50" {
		t.Errorf("limits: %q", out)
	}
	if out, err := exec.Command(self, nprocShim, "50", "--", "no-such-command-x").CombinedOutput(); err == nil || !strings.Contains(string(out), "nproc shim") {
		t.Errorf("a missing command must fail: %v %s", err, out)
	}
}
