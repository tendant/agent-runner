package supervisor

import (
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox/lease"
)

func sleeper(t *testing.T) (*exec.Cmd, lease.ProcRef) {
	c := exec.Command("sleep", "60")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-c.Process.Pid, syscall.SIGKILL); c.Wait() })
	return c, lease.ProcRef{PID: c.Process.Pid, Start: lease.ProcStart(c.Process.Pid)}
}

func exited(c *exec.Cmd) bool {
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

type rec struct {
	mu sync.Mutex
	ev []string
}

func (r *rec) add(k string, _ lease.Record) { r.mu.Lock(); r.ev = append(r.ev, k); r.mu.Unlock() }
func (r *rec) has(k string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.ev {
		if e == k {
			return true
		}
	}
	return false
}

func setup(t *testing.T) (*lease.Store, *Supervisor, *rec) {
	st, _ := lease.NewStore(t.TempDir())
	r := &rec{}
	return st, &Supervisor{Store: st, Grace: 300 * time.Millisecond, OnEvent: r.add}, r
}

func TestDeadlineKillsEvenWithLiveOwner(t *testing.T) {
	st, s, r := setup(t)
	c, ref := sleeper(t)
	me := lease.SelfOwner("A")
	st.Acquire(lease.Record{Name: "sandbox:r1", Kind: "sandbox", Owner: me, Procs: []lease.ProcRef{ref},
		Deadline: time.Now().Add(-time.Second)}, time.Hour)
	if n, _ := s.Sweep(); n != 1 {
		t.Fatalf("acted %d", n)
	}
	if !exited(c) || !r.has(EvTimeout) {
		t.Error("deadline must kill the sandbox process group")
	}
	// Owner alive: lease stays so the owner can observe and finish.
	if recs, _ := st.List(); len(recs) != 1 {
		t.Errorf("lease should remain for live owner: %d", len(recs))
	}
}

func TestOrphanReapedOnlyWhenOwnerDead(t *testing.T) {
	st, s, r := setup(t)
	c, ref := sleeper(t)
	dead := exec.Command("true")
	dead.Start()
	owner := lease.Owner{InstanceID: "old", PID: dead.Process.Pid, Start: lease.ProcStart(dead.Process.Pid)}
	dead.Wait()
	var orphaned []string
	s.OnOrphan = func(rc lease.Record) { orphaned = append(orphaned, rc.Name) }
	st.Acquire(lease.Record{Name: "sandbox:r2", Kind: "sandbox", Owner: owner, Procs: []lease.ProcRef{ref}}, time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	s.Sweep()
	if !exited(c) || !r.has(EvOrphan) || len(orphaned) != 1 {
		t.Errorf("orphan not reaped: events=%v orphaned=%v", r.ev, orphaned)
	}
	if recs, _ := st.List(); len(recs) != 0 {
		t.Error("orphan lease must be removed")
	}
}

func TestLiveOwnerWithDifferentInstanceIsNeverReaped(t *testing.T) {
	st, s, r := setup(t)
	c, ref := sleeper(t)
	// Expired lease, owner (this test process) alive, instance ID unlike any "current" one.
	st.Acquire(lease.Record{Name: "sandbox:r3", Kind: "sandbox", Owner: lease.SelfOwner("some-other-instance"), Procs: []lease.ProcRef{ref}}, time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	s.Sweep()
	if syscall.Kill(c.Process.Pid, 0) != nil {
		t.Fatal("a live runner's sandbox was killed")
	}
	if !r.has(EvStale) {
		t.Error("expected stale report for expired lease with live owner")
	}
}

func TestPIDReuseNotKilled(t *testing.T) {
	st, s, _ := setup(t)
	c, ref := sleeper(t)
	ref.Start = "not-the-real-start-time" // identity mismatch simulates pid reuse
	st.Acquire(lease.Record{Name: "sandbox:r4", Kind: "sandbox", Owner: lease.SelfOwner("A"), Procs: []lease.ProcRef{ref},
		Deadline: time.Now().Add(-time.Second)}, time.Hour)
	s.Sweep()
	time.Sleep(500 * time.Millisecond)
	if syscall.Kill(c.Process.Pid, 0) != nil {
		t.Fatal("unrelated process with reused pid was killed")
	}
}
