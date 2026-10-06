package sandbox

import (
	"fmt"
	"sort"
	"strings"
)

// Layer names, outermost first. Each layer may only tighten the one before.
const (
	LayerSystem    = "system"
	LayerBot       = "bot"
	LayerAgent     = "agent"
	LayerWorkspace = "workspace"
	LayerRun       = "run"
)

// Layer is one named policy source.
type Layer struct {
	Name string
	Spec SandboxSpec
}

// Resolve folds layers (System → Bot → Agent → Workspace → Run) into one spec.
// Unset fields inherit; a layer that tries to widen anything is an error naming
// the layer and field (fail closed). Deny lists and requirements only grow;
// secrets can only be granted by the first layer and later layers may drop them.
func Resolve(layers ...Layer) (SandboxSpec, error) {
	if len(layers) == 0 {
		return SandboxSpec{}, fmt.Errorf("sandbox resolve: no policy layers")
	}
	for _, l := range layers {
		if err := l.Spec.Validate(); err != nil {
			return SandboxSpec{}, fmt.Errorf("layer %s: %w", l.Name, err)
		}
	}
	cur := layers[0].Spec
	for _, l := range layers[1:] {
		next, err := tighten(cur, l.Spec)
		if err != nil {
			return SandboxSpec{}, fmt.Errorf("layer %s widens policy: %w", l.Name, err)
		}
		cur = next
	}
	return finalize(cur), nil
}

// finalize applies fail-closed defaults to still-unset fields.
func finalize(s SandboxSpec) SandboxSpec {
	if s.Network.Egress == "" {
		s.Network.Egress = EgressNone
	}
	if s.Network.DNS == "" {
		switch s.Network.Egress {
		case EgressNone:
			s.Network.DNS = DNSNone
		case EgressRestricted:
			s.Network.DNS = DNSControlled
		default:
			s.Network.DNS = DNSSystem
		}
	}
	return s
}

func tighten(p, c SandboxSpec) (SandboxSpec, error) {
	out := p
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	// Filesystem: deny unions; grants must be covered by a parent grant.
	if c.Filesystem.Grants != nil {
		for _, g := range c.Filesystem.Grants {
			if !grantCovered(g, p.Filesystem.Grants) {
				bad("filesystem grant %s:%s not covered by parent", g.Path, g.Access)
			}
		}
		out.Filesystem.Grants = c.Filesystem.Grants
	}
	out.Filesystem.Deny = unionSorted(p.Filesystem.Deny, c.Filesystem.Deny)

	// Network.
	pe, ce := effEgress(p.Network.Egress), c.Network.Egress
	if ce == "" {
		ce = p.Network.Egress
	}
	if effEgress(ce).rank() > pe.rank() {
		bad("egress %s exceeds %s", ce, pe)
	}
	out.Network.Egress = ce
	switch {
	case effEgress(ce) != EgressRestricted:
		out.Network.EgressAllow = nil
	case c.Network.EgressAllow != nil:
		if pe == EgressRestricted {
			if ok, h := hostsSubset(c.Network.EgressAllow, p.Network.EgressAllow); !ok {
				bad("egress_allow %q not covered by parent", h)
			}
		}
		out.Network.EgressAllow = c.Network.EgressAllow
	}
	if c.Network.DNS != "" {
		if effDNS(p.Network.DNS, p.Network.Egress).rank() < c.Network.DNS.rank() {
			bad("dns %s exceeds %s", c.Network.DNS, effDNS(p.Network.DNS, p.Network.Egress))
		}
		out.Network.DNS = c.Network.DNS
	}
	if c.Network.Listen != nil {
		if *c.Network.Listen && !listens(p.Network) {
			bad("listen not permitted by parent")
		}
		out.Network.Listen = boolPtr(*c.Network.Listen && listens(p.Network))
	}
	if c.Network.Expose != nil {
		allowed := map[int]bool{}
		for _, x := range p.Network.Expose {
			allowed[x] = true
		}
		for _, x := range c.Network.Expose {
			if !allowed[x] {
				bad("expose port %d not permitted by parent", x)
			}
		}
		out.Network.Expose = c.Network.Expose
	}

	// Resources: child must not exceed a set parent limit.
	pr, cr := p.Resources, c.Resources
	lim := func(name string, pv, cv int64) int64 {
		if cv == 0 {
			return pv
		}
		if pv != 0 && cv > pv {
			bad("resource %s %d exceeds %d", name, cv, pv)
			return pv
		}
		return cv
	}
	out.Resources.MemoryBytes = lim("memory_bytes", pr.MemoryBytes, cr.MemoryBytes)
	out.Resources.DiskBytes = lim("disk_bytes", pr.DiskBytes, cr.DiskBytes)
	out.Resources.PIDs = lim("pids", pr.PIDs, cr.PIDs)
	out.Resources.OutputBytes = lim("output_bytes", pr.OutputBytes, cr.OutputBytes)
	out.Resources.TimeoutSec = lim("timeout_sec", pr.TimeoutSec, cr.TimeoutSec)
	out.Resources.GraceSec = lim("grace_sec", pr.GraceSec, cr.GraceSec)
	switch {
	case cr.CPUs == 0:
	case pr.CPUs != 0 && cr.CPUs > pr.CPUs:
		bad("resource cpus %v exceeds %v", cr.CPUs, pr.CPUs)
	default:
		out.Resources.CPUs = cr.CPUs
	}

	// Secrets: may only drop or keep identical grants.
	if c.Secrets != nil {
		pm := map[string]SecretGrant{}
		for _, s := range p.Secrets {
			pm[s.Name] = s
		}
		for _, s := range c.Secrets {
			if pm[s.Name] != s {
				bad("secret %q not granted by parent", s.Name)
			}
		}
		out.Secrets = c.Secrets
	}

	// Requirements only accumulate.
	var req []string
	for _, r := range append(append([]CapabilityID{}, p.Requirements...), c.Requirements...) {
		req = append(req, string(r))
	}
	out.Requirements = nil
	for _, r := range unionSorted(req, nil) {
		out.Requirements = append(out.Requirements, CapabilityID(r))
	}

	if len(errs) > 0 {
		return SandboxSpec{}, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

func effEgress(m EgressMode) EgressMode {
	if m == "" {
		return EgressNone
	}
	return m
}

func effDNS(d DNSMode, e EgressMode) DNSMode {
	if d != "" {
		return d
	}
	return finalize(SandboxSpec{Network: NetworkPolicy{Egress: e}}).Network.DNS
}

func grantCovered(g PathGrant, parents []PathGrant) bool {
	for _, p := range parents {
		if pathWithin(g.Path, p.Path) && (p.Access == ReadWrite || g.Access == ReadOnly) {
			return true
		}
	}
	return false
}

func unionSorted(a, b []string) []string {
	m := map[string]bool{}
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		m[s] = true
	}
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// FSAccess evaluates the resolved filesystem policy for a logical path:
// deny wins, then the longest covering grant decides.
func (f FilesystemPolicy) FSAccess(p string, write bool) bool {
	for _, d := range f.Deny {
		if pathWithin(p, d) {
			return false
		}
	}
	best := -1
	var mode Access
	for _, g := range f.Grants {
		if pathWithin(p, g.Path) && len(g.Path) > best {
			best, mode = len(g.Path), g.Access
		}
	}
	if best < 0 {
		return false
	}
	return !write || mode == ReadWrite
}

func listens(n NetworkPolicy) bool { return n.Listen != nil && *n.Listen }

func boolPtr(b bool) *bool { return &b }
