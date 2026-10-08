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
	"sort"
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
	PolicyFile    string // optional system-layer SandboxSpec: a JSON file path, or inline JSON (see loadPolicy)
	EvidenceFile  string // optional conformance report; claims limited to proven caps
	EnvAllow      []string
	// PrivatePaths are the runner's own host files and directories (its .env
	// files, state, logs, ...). Every run is read-denied them, except for the
	// parts that hold the run's own workspace, home and tmp (see readDeny).
	PrivatePaths []string
	// Claude seeds a sandboxed claude CLI's config dir (see seedClaudeConfig).
	Claude   ClaudeSeed
	LeaseTTL time.Duration
	MaxQueue int
	OnEvent  func(Event)
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
			// No deny list by default: the sandbox home is a private runner-managed
			// directory (no real credentials), and the isobox adapter always
			// read-denies the host user's credential directories. A deny inside a
			// writable grant cannot be enforced for writes, so it would make
			// fs.deny a gap under strict.
		},
		Network: sandbox.NetworkPolicy{Egress: sandbox.EgressOutbound},
	}
}

// loadPolicy reads the system-layer policy from AGENT_SANDBOX_POLICY: a path
// to a JSON SandboxSpec, or the JSON itself when the value starts with "{".
// Fields the policy leaves unset keep DefaultSystemSpec's values, so a policy
// that only sets the network still grants the workspace, home and tmp; a set
// field replaces the default's (grants replace the default grants).
func loadPolicy(value string) (sandbox.SandboxSpec, error) {
	data, src := []byte(value), "AGENT_SANDBOX_POLICY"
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		b, err := os.ReadFile(value)
		if err != nil {
			return sandbox.SandboxSpec{}, fmt.Errorf("sandbox policy: %w", err)
		}
		data, src = b, value
	}
	spec, err := sandbox.ParseSpec(data)
	if err != nil {
		return sandbox.SandboxSpec{}, fmt.Errorf("sandbox policy %s: %w", src, err)
	}
	return withDefaults(spec, DefaultSystemSpec()), nil
}

// withDefaults fills the fields spec leaves unset from def.
func withDefaults(spec, def sandbox.SandboxSpec) sandbox.SandboxSpec {
	if len(spec.Filesystem.Grants) == 0 {
		spec.Filesystem.Grants = def.Filesystem.Grants
	}
	if len(spec.Filesystem.Deny) == 0 {
		spec.Filesystem.Deny = def.Filesystem.Deny
	}
	n, dn := &spec.Network, def.Network
	if n.Egress == "" {
		n.Egress = dn.Egress
	}
	if n.EgressAllow == nil {
		n.EgressAllow = dn.EgressAllow
	}
	if n.DNS == "" {
		n.DNS = dn.DNS
	}
	if n.Listen == nil {
		n.Listen = dn.Listen
	}
	if n.Expose == nil {
		n.Expose = dn.Expose
	}
	res, dr := &spec.Resources, def.Resources
	if res.CPUs == 0 {
		res.CPUs = dr.CPUs
	}
	if res.MemoryBytes == 0 {
		res.MemoryBytes = dr.MemoryBytes
	}
	if res.DiskBytes == 0 {
		res.DiskBytes = dr.DiskBytes
	}
	if res.PIDs == 0 {
		res.PIDs = dr.PIDs
	}
	if res.OutputBytes == 0 {
		res.OutputBytes = dr.OutputBytes
	}
	if res.TimeoutSec == 0 {
		res.TimeoutSec = dr.TimeoutSec
	}
	if res.GraceSec == 0 {
		res.GraceSec = dr.GraceSec
	}
	if spec.Secrets == nil {
		spec.Secrets = def.Secrets
	}
	if spec.Requirements == nil {
		spec.Requirements = def.Requirements
	}
	return spec
}

// Runtime creates per-run sandboxes.
type Runtime struct {
	cfg      Config
	store    *lease.Store
	threads  *lease.ThreadRuns
	owner    lease.Owner
	system   sandbox.SandboxSpec
	evidence *conformance.Report
	initErr  error
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
		if r.system, err = loadPolicy(cfg.PolicyFile); err != nil {
			return nil, err
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

// Failed returns a runtime that rejects every run with err. It is what a
// misconfigured sandbox degrades to: never silently to host execution.
func Failed(err error) *Runtime {
	return &Runtime{cfg: Config{Mode: sandbox.ModeStrict}, initErr: err}
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
	if r.initErr != nil {
		return nil, &RejectedError{r.initErr}
	}
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
	// claude's config dir lives in the sandbox home: writable, kept per thread
	// (a task's next turn resumes its conversation), seeded from an allowlist.
	claudeDir := filepath.Join(roots[sandbox.RootHome], ".claude")
	if err := seedClaudeConfig(r.cfg.Claude, claudeDir); err != nil {
		r.emit(Event{Kind: "sandbox.rejected", RunID: req.RunID, ThreadID: req.ThreadID, Detail: "claude config: " + err.Error()})
		return nil, &RejectedError{fmt.Errorf("seed claude config: %w", err)}
	}

	keep := make([]string, 0, len(roots))
	for _, d := range roots {
		keep = append(keep, d)
	}
	deny := readDeny(r.cfg.PrivatePaths, keep)
	newBackend := func(roots map[string]string) sandbox.Backend {
		if r.cfg.NewBackend != nil {
			return r.cfg.NewBackend(roots)
		}
		return isobox.New(isobox.Config{Binary: r.cfg.IsoboxBin, Backend: r.cfg.IsoboxBackend, Roots: roots, Evidence: r.evidence, ReadDeny: deny})
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
		// A `claude setup-token` token reaches the claude CLI only; the host's
		// `claude login` (Keychain, ~/.claude) is out of the sandbox's reach.
		CLIEnvAllow: map[string][]string{"claude": {"CLAUDE_CODE_OAUTH_TOKEN"}},
		EnvOverride: []string{
			"HOME=" + roots[sandbox.RootHome],
			"TMPDIR=" + roots[sandbox.RootTmp],
		},
		EnvForce: []string{"CLAUDE_CONFIG_DIR=" + claudeDir},
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

// readDeny returns the host paths a run is read-denied: each private path, or,
// for one that holds a directory the run needs (keep: its workspace, home and
// tmp), that path's other entries, descending until only the kept directory is
// left readable. So under STATE_ROOT the run's own sandbox home stays readable
// while other threads' homes, the leases and the session journal are denied.
// Entries created after the run starts (another session's new workspace) are
// not covered. Missing paths are skipped; paths are absolute and, where they
// go through a symlink (macOS /tmp), listed both ways.
func readDeny(private, keep []string) []string {
	canon := func(p string) []string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil
		}
		out := []string{abs}
		if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
			out = append(out, real)
		}
		return out
	}
	var kept []string
	for _, k := range keep {
		kept = append(kept, canon(k)...)
	}
	within := func(p, dir string) bool {
		return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
	}
	seen := map[string]bool{}
	var out []string
	var walk func(p string)
	walk = func(p string) {
		if _, err := os.Lstat(p); err != nil {
			return
		}
		holds := false
		for _, k := range kept {
			if within(p, k) {
				return // the run's own directory, or inside it
			}
			if within(k, p) {
				holds = true
			}
		}
		if !holds {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			return
		}
		ents, err := os.ReadDir(p)
		if err != nil {
			return
		}
		for _, e := range ents {
			walk(filepath.Join(p, e.Name()))
		}
	}
	for _, p := range private {
		for _, c := range canon(p) {
			walk(c)
		}
	}
	sort.Strings(out)
	return out
}
