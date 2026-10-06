package sandbox

import (
	"fmt"
	"net/netip"
	"strings"
)

type hostKind int

const (
	hostExact hostKind = iota
	hostWildcard
	hostPrefix // CIDR or single IP
	hostSet    // @name trusted set, opaque
)

type hostPattern struct {
	kind   hostKind
	name   string // exact host, wildcard suffix ("github.com" for *.github.com), or set name
	prefix netip.Prefix
}

// parseHostPattern parses an egress grant: exact host, single-label wildcard
// (*.example.com), IP/CIDR, or trusted set (@vcs).
func parseHostPattern(s string) (hostPattern, error) {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	switch {
	case s == "":
		return hostPattern{}, fmt.Errorf("empty host pattern")
	case strings.HasPrefix(s, "@"):
		if len(s) == 1 || strings.ContainsAny(s[1:], "@*/ ") {
			return hostPattern{}, fmt.Errorf("bad set %q", s)
		}
		return hostPattern{kind: hostSet, name: s}, nil
	case strings.Contains(s, "/"):
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return hostPattern{}, fmt.Errorf("bad CIDR %q", s)
		}
		return hostPattern{kind: hostPrefix, prefix: p.Masked()}, nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return hostPattern{kind: hostPrefix, prefix: netip.PrefixFrom(a, a.BitLen())}, nil
	}
	if strings.HasPrefix(s, "*.") {
		rest := s[2:]
		if rest == "" || strings.Contains(rest, "*") || !strings.Contains(rest, ".") {
			return hostPattern{}, fmt.Errorf("bad wildcard %q (only single leading *. over a domain)", s)
		}
		return hostPattern{kind: hostWildcard, name: rest}, nil
	}
	if strings.ContainsAny(s, "*: ") {
		return hostPattern{}, fmt.Errorf("bad host %q", s)
	}
	return hostPattern{kind: hostExact, name: s}, nil
}

// subsetOf reports whether every destination matched by c is matched by p.
func (c hostPattern) subsetOf(p hostPattern) bool {
	switch p.kind {
	case hostSet:
		return c.kind == hostSet && c.name == p.name
	case hostPrefix:
		return c.kind == hostPrefix && c.prefix.Bits() >= p.prefix.Bits() &&
			c.prefix.Addr().BitLen() == p.prefix.Addr().BitLen() && p.prefix.Contains(c.prefix.Addr())
	case hostExact:
		return c.kind == hostExact && c.name == p.name
	case hostWildcard:
		switch c.kind {
		case hostWildcard:
			return c.name == p.name
		case hostExact:
			label, ok := strings.CutSuffix(c.name, "."+p.name)
			return ok && label != "" && !strings.Contains(label, ".")
		}
	}
	return false
}

// hostsSubset reports whether every pattern in child is covered by some parent pattern.
func hostsSubset(child, parent []string) (bool, string) {
	var ps []hostPattern
	for _, s := range parent {
		p, err := parseHostPattern(s)
		if err != nil {
			return false, s
		}
		ps = append(ps, p)
	}
	for _, s := range child {
		c, err := parseHostPattern(s)
		if err != nil {
			return false, s
		}
		ok := false
		for _, p := range ps {
			if c.subsetOf(p) {
				ok = true
				break
			}
		}
		if !ok {
			return false, s
		}
	}
	return true, ""
}
