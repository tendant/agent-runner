// Package isobox adapts the isobox CLI (github.com/can1357/isobox) to
// sandbox.Backend. Policy stays in agent-runner: this package only translates a
// resolved SandboxSpec into isobox flags, reports what isobox enforces, and runs
// the command. isobox is used as a pinned subprocess (it needs a newer Go than
// agent-runner and `--print` gives an offline, side-effect-free Check).
package isobox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/conformance"
)

// Config configures the adapter.
type Config struct {
	Binary  string            // path to isobox; "isobox" if empty
	Backend string            // "", "seatbelt", "gvisor", ...; "" lets isobox choose
	Roots   map[string]string // logical root -> host directory
	// Evidence is the conformance report for this backend on this host. When set,
	// only capabilities it proves are claimed.
	Evidence *conformance.Report
	// RequireEvidence makes Check fail without Evidence (production).
	RequireEvidence bool
	// DefaultGrace is the TERM->KILL grace when the spec sets none.
	DefaultGrace time.Duration
}

// Backend implements sandbox.Backend over the isobox CLI.
type Backend struct{ cfg Config }

// New returns a backend.
func New(cfg Config) *Backend {
	if cfg.Binary == "" {
		cfg.Binary = "isobox"
	}
	if cfg.DefaultGrace == 0 {
		cfg.DefaultGrace = 5 * time.Second
	}
	return &Backend{cfg: cfg}
}

func (b *Backend) host(logical string) (string, error) {
	best, hp := "", ""
	for root, h := range b.cfg.Roots {
		if (logical == root || strings.HasPrefix(logical, root+"/")) && len(root) > len(best) {
			best, hp = root, h
		}
	}
	if best == "" {
		return "", fmt.Errorf("no host mapping for logical path %q", logical)
	}
	return filepath.Join(hp, strings.TrimPrefix(logical, best)), nil
}

// credentialDirs are always read-denied (relative to the real user home).
var credentialDirs = []string{".ssh", ".aws", ".gnupg", ".azure", ".docker", ".kube", ".npmrc", ".pypirc", ".netrc",
	".git-credentials", ".config/gcloud", ".config/gh", ".claude", ".codex"}

// flags translates the spec. It returns isobox flags (before "--") plus notes
// about parts of the spec isobox cannot express.
func (b *Backend) flags(spec sandbox.SandboxSpec) (flags []string, notes []string, err error) {
	flags = []string{"--profile=none"} // never inherit isobox's agent-profile defaults
	switch e := spec.Network.Egress; e {
	case sandbox.EgressOutbound:
		if spec.Network.Listen != nil && *spec.Network.Listen {
			flags = append(flags, "--net=enable")
		} else {
			flags = append(flags, "--net=outbound")
		}
	case sandbox.EgressRestricted:
		// isobox has no allowlist. Fail safe: no network at all.
		flags = append(flags, "--net=disable")
		notes = append(notes, "restricted egress translated to net=disable (no gateway yet)")
	default:
		flags = append(flags, "--net=disable")
	}

	var writable []string
	for _, g := range spec.Filesystem.Grants {
		if g.Access != sandbox.ReadWrite {
			continue
		}
		h, err := b.host(g.Path)
		if err != nil {
			return nil, nil, err
		}
		writable = append(writable, h)
	}
	sort.Strings(writable)
	if len(writable) == 0 {
		flags = append(flags, "--write=none")
	} else {
		flags = append(flags, "--write=scope")
		for _, w := range writable {
			flags = append(flags, "--writable", w)
		}
	}
	notes = append(notes, "reads are broad host reads minus denied paths (fs read scoping not used)")

	deny := map[string]bool{}
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range credentialDirs {
			deny[filepath.Join(home, d)] = true
		}
	}
	for _, d := range spec.Filesystem.Deny {
		h, err := b.host(d)
		if err != nil {
			return nil, nil, err
		}
		deny[h] = true
		for _, w := range writable {
			if h == w || strings.HasPrefix(h, w+string(filepath.Separator)) {
				notes = append(notes, "deny inside writable grant: "+d)
			}
		}
	}
	var dl []string
	for d := range deny {
		dl = append(dl, d)
	}
	sort.Strings(dl)
	for _, d := range dl {
		flags = append(flags, "--read-deny", d)
	}

	r := spec.Resources
	if r.CPUs > 0 {
		flags = append(flags, "--cpus", strconv.FormatFloat(r.CPUs, 'f', -1, 64))
	}
	if r.MemoryBytes > 0 {
		flags = append(flags, "--memory", strconv.FormatInt(r.MemoryBytes, 10))
	}
	if r.PIDs > 0 {
		flags = append(flags, "--pids", strconv.FormatInt(r.PIDs, 10))
	}
	if b.cfg.Backend != "" {
		flags = append(flags, "--backend", b.cfg.Backend)
	}
	return flags, notes, nil
}

var isoboxToCap = map[string][]sandbox.CapabilityID{
	"env.scrub":        {sandbox.CapEnvScrub},
	"fs.write.scope":   {sandbox.CapFSWriteScope},
	"fs.read.deny":     {sandbox.CapFSDeny},
	"net.disable":      {sandbox.CapNetworkDirectNone, sandbox.CapNetworkNoListen},
	"net.outbound":     {sandbox.CapNetworkOutbound, sandbox.CapNetworkNoListen},
	"res.cpu":          {sandbox.CapResourceCPU},
	"res.memory":       {sandbox.CapResourceMemory},
	"res.pids":         {sandbox.CapResourcePIDs},
	"kernel.isolation": {sandbox.CapKernelIsolation},
}

// adapterOwned are capabilities this adapter provides itself (not isobox).
var adapterOwned = []sandbox.CapabilityID{sandbox.CapProcessContainment, sandbox.CapProcessCrossRunIsolation, sandbox.CapResourceTimeout}

// Check asks `isobox --print` what it would enforce, then subtracts what the
// adapter cannot express and (if evidence is configured) what is unproven.
func (b *Backend) Check(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.CheckResult, error) {
	if b.cfg.RequireEvidence && b.cfg.Evidence == nil {
		return sandbox.CheckResult{}, errors.New("isobox backend: conformance evidence required but missing")
	}
	flags, notes, err := b.flags(spec)
	if err != nil {
		return sandbox.CheckResult{}, err
	}
	args := append([]string{"--print", "--env-allow", "PATH"}, flags...)
	args = append(args, "--", "true")
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, b.cfg.Binary, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return sandbox.CheckResult{}, fmt.Errorf("isobox --print: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	backend, enforces, caveats := parsePlan(out.String())
	declared := map[sandbox.CapabilityID]bool{}
	for _, c := range enforces {
		for _, id := range isoboxToCap[c] {
			declared[id] = true
		}
	}
	for _, id := range adapterOwned {
		declared[id] = true
	}
	// A deny nested in a writable grant can still be written: not enforceable.
	for _, n := range notes {
		if strings.HasPrefix(n, "deny inside writable grant") {
			delete(declared, sandbox.CapFSDeny)
		}
	}
	// Restricted egress and controlled DNS need a gateway isobox does not provide.
	delete(declared, sandbox.CapNetworkRestrictedEgress)
	delete(declared, sandbox.CapNetworkControlledDNS)

	var enforced []sandbox.CapabilityID
	var proven map[sandbox.CapabilityID]bool
	if ev := b.cfg.Evidence; ev != nil {
		proven = map[sandbox.CapabilityID]bool{}
		for _, c := range ev.ProvenSet() {
			proven[c] = true
		}
	} else {
		caveats = append(caveats, "capabilities are isobox-declared and NOT verified by conformance evidence")
	}
	for id := range declared {
		if proven == nil || proven[id] {
			enforced = append(enforced, id)
		}
	}
	res := sandbox.Evaluate("isobox:"+backend, spec, enforced, append(caveats, notes...))
	for i, g := range res.Gaps {
		switch {
		case declared[g.Capability] && proven != nil:
			res.Gaps[i].Reason = "declared by isobox but not proven by conformance evidence"
		case !declared[g.Capability]:
			res.Gaps[i].Reason = "not enforced by isobox/adapter"
		}
	}
	return res, nil
}

// parsePlan extracts backend, enforced capability names and caveats from
// `isobox --print` output.
func parsePlan(s string) (backend string, enforces, caveats []string) {
	inCaveats := false
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "backend:"):
			backend = strings.TrimSpace(strings.TrimPrefix(line, "backend:"))
		case strings.HasPrefix(line, "enforces:"):
			for _, c := range strings.Split(strings.TrimPrefix(line, "enforces:"), ",") {
				if c = strings.TrimSpace(c); c != "" {
					enforces = append(enforces, c)
				}
			}
		case strings.HasPrefix(line, "caveats:"):
			inCaveats = true
		case inCaveats && strings.HasPrefix(line, "  - "):
			caveats = append(caveats, strings.TrimPrefix(line, "  - "))
		default:
			inCaveats = false
		}
	}
	return
}

// Prepare returns a per-run boundary. The spec is re-translated at Run time so
// that nothing about policy can change after Prepare.
func (b *Backend) Prepare(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.PreparedSandbox, error) {
	if _, _, err := b.flags(spec); err != nil {
		return nil, err
	}
	return &prepared{b: b, spec: spec}, nil
}

type prepared struct {
	b       *Backend
	spec    sandbox.SandboxSpec
	mu      sync.Mutex
	pids    map[int]bool
	cmds    []*exec.Cmd
	cancels []context.CancelFunc
	dead    bool
}

// buildArgs returns the isobox argv and the exact environment for a command.
func (p *prepared) buildArgs(ps sandbox.ProcSpec) (args, env []string, err error) {
	if len(ps.Args) == 0 {
		return nil, nil, errors.New("sandbox run: empty command")
	}
	flags, _, err := p.b.flags(p.spec)
	if err != nil {
		return nil, nil, err
	}
	// The runner supplies the complete environment; only those names pass.
	env = append([]string(nil), ps.Env...)
	names := map[string]bool{}
	for _, e := range env {
		names[strings.SplitN(e, "=", 2)[0]] = true
	}
	if !names["PATH"] {
		env = append(env, "PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin")
		names["PATH"] = true
	}
	var nl []string
	for n := range names {
		nl = append(nl, n)
	}
	sort.Strings(nl)
	for _, n := range nl {
		flags = append(flags, "--env-allow", n)
	}
	if ps.Tag != "" {
		flags = append(flags, "--env-allow", "sbx."+ps.Tag) // inert pattern; makes the run findable in argv
	}
	if ps.Dir != "" {
		h, err := p.b.host(ps.Dir)
		if err != nil {
			return nil, nil, err
		}
		flags = append(flags, "--dir", h)
	}
	args = append(flags, "--")
	args = append(args, ps.Args...)
	return args, env, nil
}

// Command returns an unstarted *exec.Cmd that runs ps inside the sandbox, for
// callers that need their own pipes (persistent stdio agents). Cancelling ctx
// sends TERM to the process group, then KILL after the grace period. The caller
// must call Destroy after Wait.
func (p *prepared) Command(ctx context.Context, ps sandbox.ProcSpec) (*exec.Cmd, error) {
	args, env, err := p.buildArgs(ps)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	dead := p.dead
	p.mu.Unlock()
	if dead {
		return nil, errors.New("sandbox destroyed")
	}
	grace := p.b.cfg.DefaultGrace
	if g := p.spec.Resources.GraceSec; g > 0 {
		grace = time.Duration(g) * time.Second
	}
	// The spec's wall timeout bounds a Command too (TERM, grace, KILL via Cancel).
	if t := p.spec.Resources.TimeoutSec; t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
		p.mu.Lock()
		p.cancels = append(p.cancels, cancel)
		p.mu.Unlock()
	}
	cmd := exec.CommandContext(ctx, p.b.cfg.Binary, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = ps.Stdin, ps.Stdout, ps.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pid := cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		time.AfterFunc(grace, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		return nil
	}
	cmd.WaitDelay = grace + 2*time.Second
	p.mu.Lock()
	p.cmds = append(p.cmds, cmd)
	p.mu.Unlock()
	return cmd, nil
}

func (p *prepared) Run(ctx context.Context, ps sandbox.ProcSpec) (int, error) {
	args, env, err := p.buildArgs(ps)
	if err != nil {
		return 0, err
	}
	// Hard wall-clock deadline: TERM, grace, KILL.
	runCtx := ctx
	var cancel context.CancelFunc = func() {}
	if t := p.spec.Resources.TimeoutSec; t > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
	}
	defer cancel()
	grace := p.b.cfg.DefaultGrace
	if g := p.spec.Resources.GraceSec; g > 0 {
		grace = time.Duration(g) * time.Second
	}

	cmd := exec.Command(p.b.cfg.Binary, args...)
	cmd.Env = env
	cmd.Stdin = ps.Stdin // nil => /dev/null; never the caller's terminal
	cmd.Stdout, cmd.Stderr = ps.Stdout, ps.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second

	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return 0, errors.New("sandbox destroyed")
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return 0, fmt.Errorf("isobox start: %w", err)
	}
	if p.pids == nil {
		p.pids = map[int]bool{}
	}
	p.pids[cmd.Process.Pid] = true
	p.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-runCtx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case werr = <-done:
		case <-time.After(grace):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			werr = <-done
		}
		if ctx.Err() == nil {
			return -1, fmt.Errorf("sandbox timeout after %ds", p.spec.Resources.TimeoutSec)
		}
		return -1, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, werr
}

// Destroy kills any process group still running for this sandbox.
func (p *prepared) Destroy(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	for _, c := range p.cancels {
		c()
	}
	for _, c := range p.cmds {
		if c.Process != nil {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
	}
	for pid := range p.pids {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return nil
}
