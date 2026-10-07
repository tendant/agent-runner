// Package lease provides durable, renewable leases for Thread runs and sandbox
// ownership. They are files (one per lease, under a runner-state directory) so an
// external supervisor process can see them, enforce deadlines, and reap
// orphans after a runner crash. internal/locks is in-memory and process-local,
// which is why it is not used here.
package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Owner identifies the runner process holding a lease. InstanceID alone never
// proves death: liveness is judged from PID plus process start identity.
type Owner struct {
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	Start      string `json:"start,omitempty"` // ProcStart(PID) at acquisition
}

// SelfOwner describes the current process.
func SelfOwner(instance string) Owner {
	return Owner{InstanceID: instance, PID: os.Getpid(), Start: ProcStart(os.Getpid())}
}

// ProcRef names a process group member the sandbox started.
type ProcRef struct {
	PID   int    `json:"pid"`
	Start string `json:"start,omitempty"`
}

// Record is the persisted lease.
type Record struct {
	Name     string    `json:"name"` // e.g. "thread:abc" or "sandbox:run-1"
	Kind     string    `json:"kind"` // "thread" | "sandbox"
	Owner    Owner     `json:"owner"`
	Expires  time.Time `json:"expires"`            // renewed by the owner
	Deadline time.Time `json:"deadline,omitempty"` // hard wall-clock limit; supervisor enforces
	Procs    []ProcRef `json:"procs,omitempty"`    // process-group leaders to kill
	Tag      string    `json:"tag,omitempty"`      // argv tag identifying the sandbox process group
	RunDir   string    `json:"run_dir,omitempty"`
	ThreadID string    `json:"thread_id,omitempty"`
	RunID    string    `json:"run_id,omitempty"`
}

// ErrHeld is returned when a live lease is held by someone else.
var ErrHeld = errors.New("lease held")

// ErrLost is returned by Renew when the lease was taken or removed.
var ErrLost = errors.New("lease lost")

// Store keeps leases as files under Dir.
type Store struct {
	Dir string
	Now func() time.Time
}

// NewStore creates the directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, Now: time.Now}, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func fileName(name string) string {
	r := strings.NewReplacer("/", "_", ":", "_", "..", "_")
	return r.Replace(name) + ".json"
}

func (s *Store) path(name string) string { return filepath.Join(s.Dir, fileName(name)) }

// storeLock is the one lock file all lease updates in a Store serialize on.
// A sidecar per lease could never be deleted safely (a waiter would still lock
// the unlinked file while a newcomer locks a fresh one), so each lease left a
// lock file behind for good. Lease updates are rare; one lock is enough.
const storeLock = ".lock"

// withLock serializes read-modify-write on leases via flock on the store's
// lock file.
func (s *Store) withLock(fn func() error) error {
	f, err := os.OpenFile(filepath.Join(s.Dir, storeLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *Store) read(name string) (*Record, error) {
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) write(r *Record) error {
	b, _ := json.MarshalIndent(r, "", "  ")
	tmp := s.path(r.Name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(r.Name))
}

func sameOwner(a, b Owner) bool {
	return a.PID == b.PID && a.Start == b.Start && a.InstanceID == b.InstanceID
}

// Acquire takes the lease. It succeeds when free, already ours, or expired AND
// the previous owner is dead (a stalled-but-alive owner keeps it).
func (s *Store) Acquire(r Record, ttl time.Duration) (Record, error) {
	var out Record
	err := s.withLock(func() error {
		cur, err := s.read(r.Name)
		if err != nil {
			return err
		}
		if cur != nil && !sameOwner(cur.Owner, r.Owner) {
			if cur.Expires.After(s.now()) || OwnerAlive(cur.Owner) {
				return fmt.Errorf("%w: %s by pid %d", ErrHeld, r.Name, cur.Owner.PID)
			}
		}
		r.Expires = s.now().Add(ttl)
		out = r
		return s.write(&r)
	})
	return out, err
}

// Renew extends the lease and may update procs/deadline via mutate.
func (s *Store) Renew(name string, owner Owner, ttl time.Duration, mutate func(*Record)) error {
	return s.withLock(func() error {
		cur, err := s.read(name)
		if err != nil {
			return err
		}
		if cur == nil || !sameOwner(cur.Owner, owner) {
			return ErrLost
		}
		cur.Expires = s.now().Add(ttl)
		if mutate != nil {
			mutate(cur)
		}
		return s.write(cur)
	})
}

// Release removes the lease if still ours.
func (s *Store) Release(name string, owner Owner) error {
	return s.withLock(func() error {
		cur, err := s.read(name)
		if err != nil || cur == nil {
			return err
		}
		if !sameOwner(cur.Owner, owner) {
			return ErrLost
		}
		return os.Remove(s.path(name))
	})
}

// Remove deletes a lease unconditionally (supervisor use).
func (s *Store) Remove(name string) error {
	return s.withLock(func() error {
		if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// List returns all leases.
func (s *Store) List() ([]Record, error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// OwnerAlive reports whether the owner process still exists with the same
// start identity. When identity cannot be determined it answers true: an
// ambiguous owner is never reaped.
func OwnerAlive(o Owner) bool {
	if o.PID <= 0 {
		return false
	}
	if err := syscall.Kill(o.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if o.Start == "" {
		return true
	}
	cur := ProcStart(o.PID)
	return cur == "" || cur == o.Start
}

// ProcStart returns an identity for the process start time ("" if unknown).
func ProcStart(pid int) string {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return ""
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			return ""
		}
		f := strings.Fields(s[i+1:])
		if len(f) > 19 {
			return f[19] // starttime (field 22 overall)
		}
		return ""
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
