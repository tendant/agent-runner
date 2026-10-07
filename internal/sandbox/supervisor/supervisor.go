// Package supervisor runs OUTSIDE the runner's control flow (its own process via
// cmd/sandbox-supervisor) so deadlines and orphan cleanup survive a runner crash.
// It reads sandbox leases and:
//
//   - kills sandbox process groups whose hard Deadline has passed;
//   - reaps sandboxes whose lease expired AND whose owner process is dead.
//
// It never reaps because an instance ID differs, and never touches a sandbox
// whose owner is alive (a stalled owner is reported as Stale, not killed).
package supervisor

import (
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox/lease"
)

// Event kinds reported to OnEvent.
const (
	EvTimeout  = "sandbox.timeout"
	EvOrphan   = "sandbox.orphan_reaped"
	EvStale    = "sandbox.enforcement_lost" // lease expired but owner still alive
	EvTeardown = "sandbox.teardown_incomplete"
)

// Supervisor enforces deadlines and reaps orphans.
type Supervisor struct {
	Store   *lease.Store
	Grace   time.Duration // TERM -> KILL
	Now     func() time.Time
	Kill    func(pid int, sig syscall.Signal) error // injectable; default kills the process group
	Alive   func(pid int) bool
	OnEvent func(kind string, r lease.Record)
	// OnOrphan lets the runner preserve a dead run's work (e.g. quarantine RunDir)
	// before the lease is removed.
	OnOrphan func(r lease.Record)
}

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Supervisor) event(k string, r lease.Record) {
	if s.OnEvent != nil {
		s.OnEvent(k, r)
	}
}

func (s *Supervisor) kill(pid int, sig syscall.Signal) error {
	if s.Kill != nil {
		return s.Kill(pid, sig)
	}
	return syscall.Kill(-pid, sig) // process group
}

func (s *Supervisor) alive(pid int) bool {
	if s.Alive != nil {
		return s.Alive(pid)
	}
	return syscall.Kill(pid, 0) == nil
}

// Sweep performs one pass and returns the number of sandboxes acted on.
func (s *Supervisor) Sweep() (int, error) {
	recs, err := s.Store.List()
	if err != nil {
		return 0, err
	}
	acted := 0
	now := s.now()
	for _, r := range recs {
		if r.Kind != "sandbox" {
			continue
		}
		ownerAlive := lease.OwnerAlive(r.Owner)
		switch {
		case !r.Deadline.IsZero() && now.After(r.Deadline):
			s.event(EvTimeout, r)
			s.killProcs(r)
			acted++
			if !ownerAlive {
				s.finish(r)
			}
		case r.Expires.Before(now) && !ownerAlive:
			s.event(EvOrphan, r)
			s.killProcs(r)
			s.finish(r)
			acted++
		case r.Expires.Before(now):
			s.event(EvStale, r)
		}
	}
	return acted, nil
}

func (s *Supervisor) finish(r lease.Record) {
	if s.OnOrphan != nil {
		s.OnOrphan(r)
	}
	_ = s.Store.Remove(r.Name)
}

// killProcs sends TERM, waits the grace, then KILL. A recorded process is only
// signalled if its start identity still matches (pid reuse protection).
func (s *Supervisor) killProcs(r lease.Record) {
	var targets []int
	procs := r.Procs
	if len(procs) == 0 && r.Tag != "" {
		procs = lease.FindByArgvTag(r.Tag)
	}
	for _, p := range procs {
		if p.Start != "" {
			if cur := lease.ProcStart(p.PID); cur != "" && cur != p.Start {
				continue // pid was reused by something else
			}
		}
		targets = append(targets, p.PID)
		_ = s.kill(p.PID, syscall.SIGTERM)
	}
	if len(targets) == 0 {
		return
	}
	deadline := s.now().Add(s.Grace)
	for s.now().Before(deadline) {
		all := true
		for _, pid := range targets {
			if s.alive(pid) {
				all = false
			}
		}
		if all {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range targets {
		_ = s.kill(pid, syscall.SIGKILL)
	}
	time.Sleep(100 * time.Millisecond)
	for _, pid := range targets {
		if s.alive(pid) {
			s.event(EvTeardown, r)
			return
		}
	}
}

// Run sweeps every interval until stop closes.
func (s *Supervisor) Run(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_, _ = s.Sweep()
		}
	}
}
