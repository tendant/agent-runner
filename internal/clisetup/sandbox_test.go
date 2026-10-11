package clisetup

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

func fakeHost(t *testing.T, have map[string]bool, root bool) {
	oldLook, oldEuid := lookPath, euid
	t.Cleanup(func() { lookPath, euid = oldLook, oldEuid })
	lookPath = func(name string) (string, error) {
		if have[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	euid = func() int {
		if root {
			return 0
		}
		return 1000
	}
}

func TestSandboxProblems(t *testing.T) {
	all := map[string]bool{"isobox": true, "runsc": true, "ip": true, "sysctl": true, "iptables": true}
	fakeHost(t, all, true)
	if p := SandboxProblems("off", "", "gvisor"); p != nil {
		t.Errorf("off: %v", p)
	}
	if p := SandboxProblems("strict", "", "gvisor"); p != nil {
		t.Errorf("all present: %v", p)
	}

	fakeHost(t, map[string]bool{"ip": true}, false)
	p := strings.Join(SandboxProblems("permissive", "", "gvisor"), "\n")
	for _, want := range []string{"isobox not found (isobox)", "git clone " + IsoboxRepo, "git checkout " + IsoboxRef[:7],
		"AGENT_SANDBOX=off", "runsc (gVisor) not found", "missing sysctl, iptables", "as root"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
	// Seatbelt needs only isobox.
	if p := SandboxProblems("permissive", "/opt/isobox", "seatbelt"); len(p) != 1 || !strings.Contains(p[0], "(/opt/isobox)") {
		t.Errorf("seatbelt: %v", p)
	}
}

func TestSandboxStatus(t *testing.T) {
	fakeHost(t, map[string]bool{"isobox": true, "runsc": true, "ip": true, "sysctl": true, "iptables": true}, true)
	if got := SandboxStatus("", "", "", ""); got != "off" {
		t.Errorf("off: %q", got)
	}
	if got := SandboxStatus("strict", "", "gvisor", "scoped"); got != "strict, gvisor, scoped reads ✓" {
		t.Errorf("ready: %q", got)
	}
	fakeHost(t, nil, true)
	if got := SandboxStatus("permissive", "", "seatbelt", "host"); !strings.Contains(got, "✗ every run will be rejected") || !strings.Contains(got, "isobox not found") {
		t.Errorf("broken: %q", got)
	}
}

// The install help must name the isobox CI builds.
func TestIsoboxPinMatchesCI(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/sandbox.yml")
	if err != nil {
		t.Fatal(err)
	}
	repo := regexp.MustCompile(`(?m)^  ISOBOX_REPO: (\S+)`).FindSubmatch(b)
	ref := regexp.MustCompile(`(?m)^  ISOBOX_REF: ([0-9a-f]+)`).FindSubmatch(b)
	if repo == nil || ref == nil || string(repo[1]) != IsoboxRepo || string(ref[1]) != IsoboxRef {
		t.Errorf("clisetup pins %s@%s; the workflow pins %q@%q", IsoboxRepo, IsoboxRef, repo, ref)
	}
}
