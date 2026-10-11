// Package agentbox adapts agentbox (github.com/tendant/agentbox: rootless
// gVisor, no root at run time) to agent-runner's sandbox.Backend.
//
// Each Prepare creates one agentbox sandbox; policy grants are mounted at
// their own host paths, so the CLIs see the same HOME, workspace and tmp
// paths as with isobox. The sandbox has no network: model proxies listen on
// Unix sockets mounted into it, and a forwarder (fwd.go) gives the CLIs the
// http://127.0.0.1:<port> they expect.
package agentbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	ab "github.com/tendant/agentbox/pkg/agentbox"
	abgvisor "github.com/tendant/agentbox/pkg/agentbox/gvisor"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/conformance"
)

// Config configures the adapter for one run.
type Config struct {
	Roots    map[string]string // logical root -> host directory
	Readable []string          // more host paths readable (memory, uploads, the CLIs' installs)
	// ReadDeny are host paths the sandbox must never read (the runner's .env,
	// state, logs, other sessions): covered wherever a mount exposes them,
	// e.g. through a broad Readable parent.
	ReadDeny []string
	// Routes maps an in-sandbox address (127.0.0.1:4801) to a model proxy's
	// Unix socket on the host.
	Routes map[string]string
	// Evidence is the conformance report for this backend on this host;
	// when set, only capabilities it proves are claimed.
	Evidence *conformance.Report
	Runsc    string // default: agentbox's (/usr/local/bin/runsc, else PATH)
	StateDir string // agentbox state (runsc state, bundles); private to the runner
	// Backend overrides the agentbox backend (tests).
	Backend ab.Backend
}

// Backend implements sandbox.Backend over agentbox.
type Backend struct {
	cfg Config
	ab  ab.Backend
}

// New returns the adapter.
func New(cfg Config) *Backend {
	b := cfg.Backend
	if b == nil {
		b = abgvisor.New(abgvisor.Config{Runsc: cfg.Runsc, StateDir: cfg.StateDir})
	}
	return &Backend{cfg: cfg, ab: b}
}

// capsCache: Capabilities runs runsc once; the host does not change during
// a process's life.
var (
	capsMu    sync.Mutex
	capsCache = map[ab.Backend]ab.Capabilities{}
)

func (b *Backend) caps(ctx context.Context) (ab.Capabilities, error) {
	capsMu.Lock()
	defer capsMu.Unlock()
	if c, ok := capsCache[b.ab]; ok {
		return c, nil
	}
	c, err := b.ab.Capabilities(ctx)
	if err == nil && c.Rootless {
		capsCache[b.ab] = c
	}
	return c, err
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

// Check reports what agentbox enforces for spec on this host.
func (b *Backend) Check(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.CheckResult, error) {
	c, err := b.caps(ctx)
	if err != nil {
		return sandbox.CheckResult{}, err
	}
	if !c.Rootless {
		return sandbox.CheckResult{}, fmt.Errorf("agentbox: rootless gVisor is unavailable here: %s", strings.Join(c.Notes, "; "))
	}
	declared := []sandbox.CapabilityID{
		sandbox.CapEnvScrub,                 // the environment is built from scratch
		sandbox.CapFSWriteScope,             // only granted paths are mounted writable
		sandbox.CapFSReadScope,              // nothing but grants, system paths and Readable is mounted
		sandbox.CapProcessContainment,       // closing the sandbox kills everything in it
		sandbox.CapProcessCrossRunIsolation, // one sandbox per run
		sandbox.CapResourceTimeout,
		sandbox.CapKernelIsolation,
		sandbox.CapNetworkDirectNone, // no network namespace beyond loopback
		sandbox.CapNetworkNoListen,   // nothing outside can connect in
	}
	if c.PIDLimit {
		declared = append(declared, sandbox.CapResourcePIDs)
	}
	if c.MemoryLimit {
		declared = append(declared, sandbox.CapResourceMemory)
	}
	if c.CPULimit {
		declared = append(declared, sandbox.CapResourceCPU)
	}
	// Denied paths inside grants are covered by empty mounts (agentbox
	// Spec.Deny); outside every grant they are not mounted at all.
	declared = append(declared, sandbox.CapFSDeny)
	var enforced []sandbox.CapabilityID
	var caveats []string
	if ev := b.cfg.Evidence; ev != nil {
		proven := map[sandbox.CapabilityID]bool{}
		for _, id := range ev.ProvenSet() {
			proven[id] = true
		}
		for _, id := range declared {
			if proven[id] {
				enforced = append(enforced, id)
			}
		}
	} else {
		enforced = declared
		caveats = append(caveats, "capabilities are agentbox-declared and NOT verified by conformance evidence")
	}
	caveats = append(caveats, c.Notes...)
	res := sandbox.Evaluate("agentbox:gvisor", spec, enforced, caveats)
	for i, g := range res.Gaps {
		if g.Capability == sandbox.CapNetworkOutbound {
			res.Gaps[i].Reason = "agentbox sandboxes have no network (the model proxies are reached over mounted sockets)"
		}
	}
	return res, nil
}

// Prepare creates the run's sandbox.
func (b *Backend) Prepare(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.PreparedSandbox, error) {
	ws, ok := b.cfg.Roots[sandbox.RootWorkspace]
	if !ok {
		return nil, errors.New("agentbox: no workspace root")
	}
	var rnd [6]byte
	rand.Read(rnd[:])
	s := ab.Spec{ID: "ar-" + hex.EncodeToString(rnd[:]), Workspace: ws}
	mounted := map[string]bool{}
	for _, g := range spec.Filesystem.Grants {
		h, err := b.host(g.Path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(h); err != nil {
			continue // a grant for a path that does not exist yet
		}
		m := ab.Mount{Source: h}
		if g.Access == sandbox.ReadWrite {
			s.Writable = append(s.Writable, m)
		} else {
			s.ReadOnly = append(s.ReadOnly, m)
		}
		mounted[h] = true
	}
	for _, r := range b.cfg.Readable {
		if mounted[r] || underAny(r, mounted) {
			continue
		}
		if _, err := os.Stat(r); err != nil {
			continue
		}
		s.ReadOnly = append(s.ReadOnly, ab.Mount{Source: r})
		mounted[r] = true
	}
	routes := map[string]string{}
	for addr, sock := range b.cfg.Routes {
		s.ModelSockets = append(s.ModelSockets, sock)
		routes[addr] = "/run/agentbox/" + filepath.Base(sock)
	}
	for _, d := range spec.Filesystem.Deny {
		h, err := b.host(d)
		if err != nil {
			return nil, err
		}
		s.Deny = append(s.Deny, h)
	}
	s.Deny = append(s.Deny, b.cfg.ReadDeny...)
	r := spec.Resources
	s.Limits = ab.Limits{MemoryBytes: r.MemoryBytes, CPUs: r.CPUs, PIDs: r.PIDs}
	if r.TimeoutSec > 0 {
		// Run enforces TERM, grace, KILL; the lifetime is the backstop for
		// commands handed back through Command.
		s.Lifetime = time.Duration(r.TimeoutSec+r.GraceSec+5) * time.Second
	}

	// The forwarder is this binary, copied where the sandbox can run it: the
	// run's tmp root (mounted at its host path).
	var fwd string
	if len(routes) > 0 {
		tmp, ok := b.cfg.Roots[sandbox.RootTmp]
		if !ok {
			return nil, errors.New("agentbox: model routes need a tmp root for the forwarder")
		}
		var err error
		if fwd, err = copySelf(tmp); err != nil {
			return nil, fmt.Errorf("agentbox: forwarder: %w", err)
		}
	}
	sb, err := b.ab.Create(ctx, s)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(r.GraceSec) * time.Second
	if grace == 0 {
		grace = 5 * time.Second
	}
	return &prepared{b: b, sb: sb, fwd: fwd, routes: routes,
		timeout: time.Duration(r.TimeoutSec) * time.Second, grace: grace}, nil
}

func underAny(p string, dirs map[string]bool) bool {
	for d := range dirs {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// copySelf copies the running binary into dir (a fresh copy, never a link:
// the sandbox can write dir).
func copySelf(dir string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, ".agent-runner-fwd")
	os.RemoveAll(dst)
	in, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	return dst, out.Close()
}

type prepared struct {
	b        *Backend
	sb       ab.Sandbox
	fwd      string
	routes   map[string]string
	timeout  time.Duration // the spec's wall timeout (0: none)
	grace    time.Duration // TERM to KILL
	mu       sync.Mutex
	cleanups []func()
}

// Command returns the command for p, confined to the sandbox, with the
// model forwarder in front when the run has model routes.
func (p *prepared) Command(ctx context.Context, ps sandbox.ProcSpec) (*exec.Cmd, error) {
	cmdr, ok := p.sb.(ab.Commander)
	if !ok {
		return nil, errors.New("agentbox: sandbox cannot hand back commands")
	}
	args := ps.Args
	if p.fwd != "" {
		args = append([]string{p.fwd}, fwdArgs(p.routes, ps.Args)...)
	}
	dir := ps.Dir
	if dir != "" {
		var err error
		if dir, err = p.b.host(dir); err != nil {
			return nil, err
		}
	}
	c, cleanup, err := cmdr.Cmd(ctx, ab.Command{Args: args, Env: ps.Env, Dir: dir})
	if err != nil {
		return nil, err
	}
	c.Stdin, c.Stdout, c.Stderr = ps.Stdin, ps.Stdout, ps.Stderr
	p.mu.Lock()
	p.cleanups = append(p.cleanups, cleanup)
	p.mu.Unlock()
	return c, nil
}

// Run runs p to completion; a cancelled ctx kills it inside the sandbox.
func (p *prepared) Run(ctx context.Context, ps sandbox.ProcSpec) (int, error) {
	c, err := p.Command(ctx, ps)
	if err != nil {
		return -1, err
	}
	if err := c.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var timeout <-chan time.Time
	if p.timeout > 0 {
		t := time.NewTimer(p.timeout)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case err = <-done:
	case <-timeout:
		// TERM, grace, then KILL everything the command left.
		c.Process.Signal(syscall.SIGTERM)
		select {
		case err = <-done:
		case <-time.After(p.grace):
			p.runCleanups()
			c.Process.Kill()
			err = <-done
		}
		p.runCleanups()
		return 124, nil
	case <-ctx.Done():
		p.runCleanups()
		<-done
		return -1, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	}
	return -1, err
}

func (p *prepared) runCleanups() {
	p.mu.Lock()
	cs := p.cleanups
	p.cleanups = nil
	p.mu.Unlock()
	for _, c := range cs {
		c()
	}
}

// Destroy kills what is left of the commands and closes the sandbox.
func (p *prepared) Destroy(ctx context.Context) error {
	p.runCleanups()
	if p.fwd != "" {
		defer os.Remove(p.fwd)
	}
	return p.sb.Close(ctx)
}
