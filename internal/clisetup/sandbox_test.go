package clisetup

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func fakeHost(t *testing.T, os string, have ...string) {
	oldLook, oldGOOS := lookPath, goos
	t.Cleanup(func() { lookPath, goos = oldLook, oldGOOS })
	goos = os
	lookPath = func(name string) (string, error) {
		if slices.Contains(have, name) {
			return name, nil
		}
		return "", errors.New("not found")
	}
}

func TestSandboxProblems(t *testing.T) {
	fakeHost(t, "linux", "/usr/local/bin/runsc")
	if p := SandboxProblems("off", "gvisor"); p != nil {
		t.Errorf("off: %v", p)
	}
	if p := SandboxProblems("strict", ""); p != nil {
		t.Errorf("all present: %v", p)
	}
	if p := SandboxProblems("strict", "agentbox"); p != nil {
		t.Errorf("explicit agentbox: %v", p)
	}
	// The isobox backends are gone, said plainly.
	for _, b := range []string{"gvisor", "seatbelt"} {
		if p := SandboxProblems("permissive", b); len(p) != 1 || !strings.Contains(p[0], "isobox was removed") {
			t.Errorf("%s: %v", b, p)
		}
	}
	fakeHost(t, "linux")
	p := strings.Join(SandboxProblems("permissive", ""), "\n")
	for _, want := range []string{"runsc (gVisor) not found", "github.com/tendant/agentbox", "make install", "AGENT_SANDBOX=off"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
	fakeHost(t, "darwin", "/usr/bin/sandbox-exec")
	if p := SandboxProblems("permissive", ""); p != nil {
		t.Errorf("macOS: %v", p)
	}
	fakeHost(t, "windows")
	if p := SandboxProblems("permissive", ""); len(p) != 1 {
		t.Errorf("windows: %v", p)
	}
}

func TestSandboxStatus(t *testing.T) {
	fakeHost(t, "linux", "/usr/local/bin/runsc")
	if got := SandboxStatus("", "", ""); got != "off" {
		t.Errorf("off: %q", got)
	}
	if got := SandboxStatus("strict", "", ""); !strings.HasPrefix(got, "strict, agentbox (") || !strings.HasSuffix(got, "✓") {
		t.Errorf("ready: %q", got)
	}
	fakeHost(t, "linux")
	if got := SandboxStatus("permissive", "", "host"); !strings.Contains(got, "✗ every run will be rejected") || !strings.Contains(got, "AGENT_SANDBOX_READ=host") {
		t.Errorf("broken: %q", got)
	}
}

func TestSandboxScopedReads(t *testing.T) {
	for _, v := range []string{"", "scoped"} {
		if s, err := SandboxScopedReads(v); !s || err != nil {
			t.Errorf("%q: %v %v", v, s, err)
		}
	}
	for _, v := range []string{"host", "x"} {
		if _, err := SandboxScopedReads(v); err == nil {
			t.Errorf("%q: want an error", v)
		}
	}
}
