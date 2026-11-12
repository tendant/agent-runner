// Package sandbox defines agent-runner's sandbox policy model. The runner owns
// policy; backends (agentbox) only declare and enforce capabilities. A strict
// run whose policy needs a capability the backend lacks never starts.
package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// SpecVersion is the only policy schema version this build understands.
const SpecVersion = 1

// Logical filesystem roots. Backends map them onto platform paths.
const (
	RootWorkspace = "/workspace"
	RootHome      = "/home/agent"
	RootTmp       = "/tmp"
)

var logicalRoots = []string{RootWorkspace, RootHome, RootTmp}

// Access is a filesystem grant mode.
type Access string

const (
	ReadOnly  Access = "ro"
	ReadWrite Access = "rw"
)

// EgressMode is the outbound network posture, ordered by permissiveness.
type EgressMode string

const (
	EgressNone       EgressMode = "none"
	EgressRestricted EgressMode = "restricted"
	EgressOutbound   EgressMode = "outbound"
)

func (m EgressMode) rank() int {
	switch m {
	case EgressNone:
		return 0
	case EgressRestricted:
		return 1
	case EgressOutbound:
		return 2
	}
	return -1
}

// DNSMode is the name-resolution posture, ordered by permissiveness.
type DNSMode string

const (
	DNSNone       DNSMode = "none"
	DNSControlled DNSMode = "controlled"
	DNSSystem     DNSMode = "system"
)

func (m DNSMode) rank() int {
	switch m {
	case DNSNone:
		return 0
	case DNSControlled:
		return 1
	case DNSSystem:
		return 2
	}
	return -1
}

// SandboxSpec is the single runner-owned policy for one run. Zero values mean
// "unset / inherit" when layered (see Resolve).
type SandboxSpec struct {
	Version      int              `json:"version"`
	Filesystem   FilesystemPolicy `json:"filesystem"`
	Network      NetworkPolicy    `json:"network"`
	Resources    ResourcePolicy   `json:"resources"`
	Requirements []CapabilityID   `json:"requirements,omitempty"`
}

// FilesystemPolicy lists grants and denies over logical paths. Deny always wins.
type FilesystemPolicy struct {
	Grants []PathGrant `json:"grants,omitempty"`
	Deny   []string    `json:"deny,omitempty"`
	// ScopedReads limits host reads to the grants plus the backend's
	// readable system paths; otherwise the host is readable minus Deny.
	ScopedReads bool `json:"scoped_reads,omitempty"`
}

// PathGrant allows access to a logical path subtree.
type PathGrant struct {
	Path   string `json:"path"`
	Access Access `json:"access"`
}

// NetworkPolicy keeps egress, listen, DNS and exposure separate.
type NetworkPolicy struct {
	Egress      EgressMode `json:"egress,omitempty"`
	EgressAllow []string   `json:"egress_allow,omitempty"` // hosts, *.wildcards, CIDRs/IPs, @sets
	// EgressSets are the definitions of the @sets EgressAllow names, set on
	// a run's resolved spec (UsedSets): what each meant for that run.
	EgressSets map[string][]string `json:"egress_sets,omitempty"`
	// EgressReportOnly: a host EgressAllow does not grant is reached anyway
	// and recorded as outside it, not refused (AGENT_SANDBOX_EGRESS_ENFORCE=
	// report), set on a run's resolved spec.
	EgressReportOnly bool `json:"egress_report_only,omitempty"`
	// EgressInspect: the gateway reads and records each HTTP(S) request
	// (AGENT_SANDBOX_EGRESS_INSPECT=on), except to EgressInspectExcept's
	// hosts; set on a run's resolved spec.
	EgressInspect       bool     `json:"egress_inspect,omitempty"`
	EgressInspectExcept []string `json:"egress_inspect_except,omitempty"`
	DNS                 DNSMode  `json:"dns,omitempty"`
	Listen              *bool    `json:"listen,omitempty"` // nil = inherit; root default false
	Expose              []int    `json:"expose,omitempty"`
	// Services are fixed upstreams the run may reach by name, outside the
	// egress gateway (AGENT_SANDBOX_SERVICES): a database, a cache, an
	// internal API. Each is one host:port, so granting one opens nothing
	// else, internal addresses included. nil = inherit; a run with no
	// network (egress none) has none.
	Services []Service `json:"services,omitempty"`
}

// Service is one named upstream (NetworkPolicy.Services).
type Service struct {
	Name     string `json:"name"`     // [a-z][a-z0-9_]*, at most 32: the agent's AGENT_SERVICE_<NAME>_ADDR
	Upstream string `json:"upstream"` // host:port the runner connects to
}

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Validate checks a service's name and upstream.
func (sv Service) Validate() error {
	if !serviceName.MatchString(sv.Name) {
		return fmt.Errorf("service name %q: want lowercase letters, digits and _, starting with a letter (at most 32)", sv.Name)
	}
	host, port, err := net.SplitHostPort(sv.Upstream)
	if err != nil || host == "" {
		return fmt.Errorf("service %s: upstream %q is not host:port", sv.Name, sv.Upstream)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("service %s: bad port in %q", sv.Name, sv.Upstream)
	}
	return nil
}

// ParseServices reads AGENT_SANDBOX_SERVICES: name=host:port, comma-separated.
func ParseServices(v string) ([]Service, error) {
	var out []Service
	seen := map[string]bool{}
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		name, up, ok := strings.Cut(item, "=")
		sv := Service{Name: strings.TrimSpace(name), Upstream: strings.TrimSpace(up)}
		if !ok {
			return nil, fmt.Errorf("service %q: want name=host:port", item)
		}
		if err := sv.Validate(); err != nil {
			return nil, err
		}
		if seen[sv.Name] {
			return nil, fmt.Errorf("service %s named twice", sv.Name)
		}
		seen[sv.Name] = true
		out = append(out, sv)
	}
	return out, nil
}

// ResourcePolicy caps resources; zero means unset (inherit / unlimited at root).
type ResourcePolicy struct {
	CPUs        float64 `json:"cpus,omitempty"`
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
	DiskBytes   int64   `json:"disk_bytes,omitempty"`
	PIDs        int64   `json:"pids,omitempty"`
	OutputBytes int64   `json:"output_bytes,omitempty"`
	TimeoutSec  int64   `json:"timeout_sec,omitempty"`
	GraceSec    int64   `json:"grace_sec,omitempty"` // TERM -> grace -> KILL
}

// ParseSpec decodes a spec strictly: unknown fields, trailing data and
// unsupported versions fail closed.
func ParseSpec(data []byte) (SandboxSpec, error) {
	var s SandboxSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return SandboxSpec{}, fmt.Errorf("sandbox spec: %w", err)
	}
	if dec.More() {
		return SandboxSpec{}, errors.New("sandbox spec: trailing data")
	}
	if err := s.Validate(); err != nil {
		return SandboxSpec{}, err
	}
	return s, nil
}

// Validate checks structure only; widening is Resolve's concern.
func (s SandboxSpec) Validate() error {
	if s.Version != SpecVersion {
		return fmt.Errorf("sandbox spec: unsupported version %d (want %d)", s.Version, SpecVersion)
	}
	for _, g := range s.Filesystem.Grants {
		if err := validLogicalPath(g.Path); err != nil {
			return fmt.Errorf("sandbox spec: grant: %w", err)
		}
		if g.Access != ReadOnly && g.Access != ReadWrite {
			return fmt.Errorf("sandbox spec: grant %q: bad access %q", g.Path, g.Access)
		}
	}
	for _, d := range s.Filesystem.Deny {
		if err := validLogicalPath(d); err != nil {
			return fmt.Errorf("sandbox spec: deny: %w", err)
		}
	}
	n := s.Network
	if n.Egress != "" && n.Egress.rank() < 0 {
		return fmt.Errorf("sandbox spec: bad egress mode %q", n.Egress)
	}
	if n.DNS != "" && n.DNS.rank() < 0 {
		return fmt.Errorf("sandbox spec: bad dns mode %q", n.DNS)
	}
	if len(n.EgressAllow) > 0 && n.Egress != EgressRestricted && n.Egress != "" {
		return fmt.Errorf("sandbox spec: egress_allow requires restricted egress, got %q", n.Egress)
	}
	for _, e := range n.EgressAllow {
		if err := validHostPattern(e); err != nil {
			return fmt.Errorf("sandbox spec: egress_allow: %w", err)
		}
	}
	for _, p := range n.Expose {
		if p < 1 || p > 65535 {
			return fmt.Errorf("sandbox spec: bad expose port %d", p)
		}
	}
	seenSvc := map[string]bool{}
	for _, sv := range n.Services {
		if err := sv.Validate(); err != nil {
			return fmt.Errorf("sandbox spec: %w", err)
		}
		if seenSvc[sv.Name] {
			return fmt.Errorf("sandbox spec: service %s named twice", sv.Name)
		}
		seenSvc[sv.Name] = true
	}
	r := s.Resources
	if r.CPUs < 0 || r.MemoryBytes < 0 || r.DiskBytes < 0 || r.PIDs < 0 || r.OutputBytes < 0 || r.TimeoutSec < 0 || r.GraceSec < 0 {
		return errors.New("sandbox spec: negative resource limit")
	}
	for _, c := range s.Requirements {
		if _, ok := Registry[c]; !ok {
			return fmt.Errorf("sandbox spec: unknown capability %q", c)
		}
	}
	return nil
}

// validLogicalPath requires a clean absolute path under a logical root.
func validLogicalPath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return fmt.Errorf("path %q must be absolute and clean", p)
	}
	for _, r := range logicalRoots {
		if pathWithin(p, r) {
			return nil
		}
	}
	return fmt.Errorf("path %q is outside logical roots %v", p, logicalRoots)
}

// pathWithin reports whether p equals root or lies beneath it.
func pathWithin(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}
