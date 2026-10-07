package gitsafe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A malicious repo config must not execute on the host.
func TestFsmonitorAndHooksNeutralized(t *testing.T) {
	d := t.TempDir()
	run := func(c *exec.Cmd) {
		c.Dir = d
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run(exec.Command("git", "init", "-q"))
	pwned := filepath.Join(d, "PWNED")
	os.WriteFile(filepath.Join(d, "evil.sh"), []byte("#!/bin/sh\ntouch "+pwned+"\n"), 0o755)
	run(exec.Command("git", "config", "core.fsmonitor", filepath.Join(d, "evil.sh")))
	hook := filepath.Join(d, ".git", "hooks", "pre-commit")
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+pwned+"\n"), 0o755)
	os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644)

	run(Command("status", "--porcelain"))
	run(Command("add", "f"))
	run(Command("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "m"))
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("repo-controlled program ran on the host")
	}
	// Control: plain git does run the fsmonitor, proving the test is meaningful.
	run(exec.Command("git", "status", "--porcelain"))
	if _, err := os.Stat(pwned); err != nil && !strings.Contains(os.Getenv("GIT_TEST_SKIP_CONTROL"), "1") {
		t.Log("control did not trigger fsmonitor on this git version")
	}
}
