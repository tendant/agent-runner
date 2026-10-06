package supervisor

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox/lease"
)

// With no recorded pids (runner died early), the argv tag finds the group.
func TestTagFallbackKillsGroup(t *testing.T) {
	st, s, _ := setup(t)
	c := exec.Command("sleep", "61", "sbx.run-tag-xyz")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-c.Process.Pid, syscall.SIGKILL); c.Wait() })
	st.Acquire(lease.Record{Name: "sandbox:rt", Kind: "sandbox", Owner: lease.SelfOwner("A"), Tag: "run-tag-xyz",
		Deadline: time.Now().Add(-time.Second)}, time.Hour)
	s.Sweep()
	if !exited(c) {
		t.Error("tagged process must be killed at deadline")
	}
}
