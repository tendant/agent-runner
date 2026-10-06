// Package runtime composes the sandbox pieces for one agent run:
//
//	Resolve policy -> Check (reject in strict) -> Thread lease -> sandbox lease
//	-> context carrying the SandboxLauncher -> (run) -> Finish
//
// The engine calls Begin once at the top of a run and defers Finish. Every
// agent CLI started under the returned context is confined; there is no host
// fallback unless the mode is off.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/conformance"
	"github.com/agent-runner/agent-runner/internal/sandbox/isobox"
	"github.com/agent-runner/agent-runner/internal/sandbox/lease"
)

// Event is a focused observability record. It never carries environment
// values, secrets, or payloads.
type Event struct {
	Kind     string // sandbox.started, .finished, .rejected, .enforcement_lost, .timeout ...
	RunID    string
	ThreadID string
	Backend  string
	Detail   string
}

// Config configures the runtime.
type Config struct {
	Mode          sandbox.Mode
	StateDir      string // sandbox state root (leases, per-run dirs)
	Instance      string // runner instance id
	IsoboxBin     string
	IsoboxBackend string // "", "seatbelt", "gvisor"
	PolicyFile    string // optional JSON SandboxSpec for the system layer
	EvidenceFile  string // optional conformance report; claims limited to proven caps
	EnvAllow      []string
	LeaseTTL      time.Duration
	MaxQueue      int
	OnEvent       func(Event)
	// NewBackend overrides backend construction (tests, other backends).
	NewBackend func(roots map[string]string) sandbox.Backend
}

// DefaultSystemSpec is the policy used when no PolicyFile is configured: the
// workspace, sandbox home and tmp are writable; everything else on the host is
// not; SSH material is denied; outbound network without listeners (the egress
// gateway is not enforceable on every backend yet).
func DefaultSystemSpec() sandbox.SandboxSpec {
	return sandbox.SandboxSpec{
		Version: sandbox.SpecVersion,
		Filesystem: sandbox.FilesystemPolicy{
			Grants: []sandbox.PathGrant{
				{Path: sandbox.RootWorkspace, Access: sandbox.ReadWrite},
				{Path: sandbox.RootHome, Access: sandbox.ReadWrite},
				{Path: sandbox.RootTmp, Access: sandbox.ReadWrite},
			},
			Deny: []string{sandbox.RootHome + "/.ssh"},
		},
		Network: sandbox.NetworkPolicy{Egress: sandbox.EgressOutbound},
	}
}

// Runtime creates per-run sandboxes.
type Runtime struct {
	cfg      Config
	store    *lease.Store
	threads  *lease.ThreadRuns
	owner    lease.Owner
	system   sandbox.SandboxSpec
	evidence *conformance.Report
}

// New validates configuration. Mode off returns a passthrough runtime.
func New(cfg Config) (*Runtime, error) {
	r := &Runtime{cfg: cfg}
	if cfg.Mode == sandbox.ModeOff || cfg.Mode == "" {
		r.cfg.Mode = sandbox.ModeOff
		return r, nil
	}
	if cfg.Mode != sandbox.ModeStrict && cfg.Mode != sandbox.ModePermissive {
		return nil, fmt.Errorf("sandbox: bad mode %q (want off|permissive|strict)", cfg.Mode)
	}
	if cfg.StateDir == "" {
		return nil, errors.New("sandbox: state dir required")
	}
	if cfg.LeaseTTL == 0 {
		r.cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.MaxQueue == 0 {
		r.cfg.MaxQueue = 8
	}
	st, err := lease.NewStore(filepath.Join(cfg.StateDir, "leases"))
	if err != nil {
		return nil, err
	}
	r.store = st
	r.owner = lease.SelfOwner(cfg.Instance)
	r.system = DefaultSystemSpec()
	if cfg.PolicyFile != "" {
		b, err := os.ReadFile(cfg.PolicyFile)
		if err != nil {
			return nil, err
		}
		if r.system, err = sandbox.ParseSpec(b); err != nil {
			return nil, fmt.Errorf("sandbox policy %s: %w", cfg.PolicyFile, err)
		}
	}
	if cfg.EvidenceFile != "" {
		b, err := os.ReadFile(cfg.EvidenceFile)
		if err != nil {
			return nil, err
		}
		var rep conformance.Report
		if err := json.Unmarshal(b, &rep); err != nil {
			return nil, fmt.Errorf("sandbox evidence: %w", err)
		}
		r.evidence = &rep
	}
	r.threads = &lease.ThreadRuns{Store: st, Owner: r.owner, TTL: r.cfg.LeaseTTL, MaxQueue: r.cfg.MaxQueue,
		OnLost: func(th string) {
			r.emit(Event{Kind: "sandbox.enforcement_lost", ThreadID: th, Detail: "thread lease lost"})
		}}
	return r, nil
}

// Mode reports the configured mode.
func (r *Runtime) Mode() sandbox.Mode { return r.cfg.Mode }

func (r *Runtime) emit(e Event) {
	if r.cfg.OnEvent != nil {
		r.cfg.OnEvent(e)
	}
}

// BeginReq describes one run.
type BeginReq struct {
	ThreadID  string
	RunID     string
	Workspace string          // host path of the agent's working directory
	Layers    []sandbox.Layer // bot/agent/workspace/run layers after the system layer
	MaxSecond int             // hard wall-clock for the whole run (0 = none)
}

// RejectedError is returned when a strict run cannot start.
type RejectedError struct{ Err error }

func (e *RejectedError) Error() string { return "sandbox rejected run: " + e.Err.Error() }
func (e *RejectedError) Unwrap() error { return e.Err }

// Run is a begun run. Finish must be called exactly once (it is idempotent).
type Run struct {
	Ctx      context.Context // carries the SandboxLauncher; cancelled if enforcement is lost
	Spec     sandbox.SandboxSpec
	rt       *Runtime
	req      BeginReq
	cancel   func()
	stop     chan struct{}
	once     sync.Once
	roots    map[string]string
	releaseT func()
	started  time.Time
	backend  string
}

// Begin prepares a run. With mode off it returns ctx unchanged.
func (r *Runtime) Begin(ctx context.Context, req BeginReq) (*Run, error) {
	if r.cfg.Mode == sandbox.ModeOff {
		return &Run{Ctx: ctx, rt: r, req: req, stop: make(chan struct{})}, nil
	}
	if req.RunID == "" || req.Workspace == "" {
		return nil, errors.New("sandbox: run id and workspace required")
	}
	if req.ThreadID == "" {
		req.ThreadID = req.RunID
	}
	layers := append([]sandbox.Layer{{Name: sandbox.LayerSystem, Spec: r.system}}, req.Layers...)
	spec, err := sandbox.Resolve(layers...)
	if err != nil {
		r.emit(Event{Kind: "sandbox.rejected", RunID: req.RunID, ThreadID: req.ThreadID, Detail: err.Error()})
		return nil, &RejectedError{err}
	}

	base := filepath.Join(r.cfg.StateDir, "runs", safe(req.RunID))
	roots := map[string]string{
		sandbox.RootWorkspace: req.Workspace,
		sandbox.RootHome:      filepath.Join(r.cfg.StateDir, "home", safe(req.ThreadID)),
		sandbox.RootTmp:       filepath.Join(base, "tmp"),
	}
	for _, d := range roots {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	newBackend := func(roots map[string]string) sandbox.Backend {
		if r.cfg.NewBackend != nil {
			return r.cfg.NewBackend(roots)
		}
		return isobox.New(isobox.Config{Binary: r.cfg.IsoboxBin, Backend: r.cfg.IsoboxBackend, Roots: roots, Evidence: r.evidence})
	}

	// Pre-flight: a strict run with gaps never starts (and never takes a lease).
	res, err := newBackend(roots).Check(ctx, spec)
	if err != nil {
		r.emit(Event{Kind: "sandbox.rejected", RunID: req.RunID, ThreadID: req.ThreadID, Detail: "check failed: " + err.Error()})
		return nil, &RejectedError{err}
	}
	if err := sandbox.Admit(r.cfg.Mode, res); err != nil {
		r.emit(Event{Kind: "sandbox.rejected", RunID: req.RunID, ThreadID: req.ThreadID, Backend: res.Backend, Detail: err.Error()})
		return nil, &RejectedError{err}
	}
	gapNote := ""
	if len(res.Gaps) > 0 { // permissive: run, but say what is not enforced
		var ids []string
		for _, g := range res.Gaps {
			ids = append(ids, string(g.Capability))
		}
		gapNote = "unenforced: " + strings.Join(ids, ",")
	}

	runCtx, releaseThread, err := r.threads.Enter(ctx, req.ThreadID, req.RunID)
	if err != nil {
		return nil, err
	}
	name := "sandbox:" + req.RunID
	rec := lease.Record{Name: name, Kind: "sandbox", Owner: r.owner, ThreadID: req.ThreadID, RunID: req.RunID, Tag: req.RunID, RunDir: base}
	if req.MaxSecond > 0 {
		rec.Deadline = time.Now().Add(time.Duration(req.MaxSecond)*time.Second + 30*time.Second)
	}
	if _, err := r.store.Acquire(rec, r.cfg.LeaseTTL); err != nil {
		releaseThread()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(runCtx)
	run := &Run{rt: r, req: req, Spec: spec, cancel: cancel, stop: make(chan struct{}), roots: roots, releaseT: releaseThread, started: time.Now(), backend: res.Backend}
	go func() { // keep the sandbox lease alive while the run is active
		t := time.NewTicker(r.cfg.LeaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-run.stop:
				return
			case <-t.C:
				if err := r.store.Renew(name, r.owner, r.cfg.LeaseTTL, nil); err != nil {
					r.emit(Event{Kind: "sandbox.enforcement_lost", RunID: req.RunID, ThreadID: req.ThreadID, Detail: "sandbox lease lost"})
					cancel()
					return
				}
			}
		}
	}()

	launcher := &executor.SandboxLauncher{
		Backend:  func(string) sandbox.Backend { return newBackend(roots) },
		Spec:     spec,
		Mode:     r.cfg.Mode,
		EnvAllow: r.cfg.EnvAllow,
		Tag:      req.RunID,
		EnvOverride: []string{
			"HOME=" + roots[sandbox.RootHome],
			"TMPDIR=" + roots[sandbox.RootTmp],
		},
		OnCheck: func(cr sandbox.CheckResult, err error) {
			if err != nil || (r.cfg.Mode == sandbox.ModeStrict && len(cr.Gaps) > 0) {
				r.emit(Event{Kind: "sandbox.rejected", RunID: req.RunID, ThreadID: req.ThreadID, Backend: cr.Backend, Detail: "check at launch failed"})
			}
		},
	}
	run.Ctx = executor.WithLauncher(runCtx, launcher)
	r.emit(Event{Kind: "sandbox.started", RunID: req.RunID, ThreadID: req.ThreadID, Backend: res.Backend, Detail: gapNote})
	return run, nil
}

// Finish ends the run: stops renewal, releases leases, removes the run's tmp.
func (run *Run) Finish() {
	run.once.Do(func() {
		close(run.stop)
		if run.rt.cfg.Mode == sandbox.ModeOff {
			return
		}
		if run.cancel != nil {
			run.cancel()
		}
		_ = run.rt.store.Release("sandbox:"+run.req.RunID, run.rt.owner)
		_ = os.RemoveAll(filepath.Dir(run.roots[sandbox.RootTmp])) // runs/<id>
		if run.releaseT != nil {
			run.releaseT()
		}
		run.rt.emit(Event{Kind: "sandbox.finished", RunID: run.req.RunID, ThreadID: run.req.ThreadID, Backend: run.backend,
			Detail: fmt.Sprintf("elapsed=%s", time.Since(run.started).Round(time.Millisecond))})
	})
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '.' && false {
			return '_'
		}
		return r
	}, strings.ReplaceAll(s, "..", "_"))
}
