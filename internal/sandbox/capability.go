package sandbox

import "sort"

// CapabilityID names one enforceable property in the versioned registry.
type CapabilityID string

const (
	CapFSDeny                   CapabilityID = "fs.deny"
	CapFSWriteScope             CapabilityID = "fs.write_scope"
	CapFSPathVirtualization     CapabilityID = "fs.path_virtualization"
	CapEnvScrub                 CapabilityID = "env.scrub"
	CapProcessContainment       CapabilityID = "process.containment"
	CapProcessCrossRunIsolation CapabilityID = "process.cross_run_isolation"
	CapNetworkDirectNone        CapabilityID = "network.direct_none"
	CapNetworkOutbound          CapabilityID = "network.outbound"
	CapNetworkNoListen          CapabilityID = "network.no_listen"
	CapNetworkRestrictedEgress  CapabilityID = "network.restricted_egress"
	CapNetworkControlledDNS     CapabilityID = "network.controlled_dns"
	CapResourceCPU              CapabilityID = "resource.cpu"
	CapResourceMemory           CapabilityID = "resource.memory"
	CapResourcePIDs             CapabilityID = "resource.pids"
	CapResourceDisk             CapabilityID = "resource.disk"
	CapResourceOutput           CapabilityID = "resource.output_bytes"
	CapResourceTimeout          CapabilityID = "resource.timeout"
	CapDeadlineSurvivesRunner   CapabilityID = "resource.deadline_survives_runner"
	CapOrphanRecovery           CapabilityID = "runtime.orphan_recovery"
	CapKernelIsolation          CapabilityID = "kernel.isolation"
)

// RegistryVersion bumps whenever a capability's definition changes.
const RegistryVersion = 1

// CapabilityDef is a precise definition. A backend may claim the capability
// only if every named conformance test passes (see sandbox/conformance).
type CapabilityDef struct {
	ID          CapabilityID
	Description string
	Tests       []string
}

func def(id CapabilityID, desc string, tests ...string) CapabilityDef {
	return CapabilityDef{ID: id, Description: desc, Tests: tests}
}

// Registry is the one source of capability definitions.
var Registry = func() map[CapabilityID]CapabilityDef {
	m := map[CapabilityID]CapabilityDef{}
	for _, d := range []CapabilityDef{
		def(CapFSDeny, "denied paths are unreadable and unwritable, including via symlinks", "fs.ssh_unreadable", "fs.symlink_escape"),
		def(CapFSWriteScope, "writes succeed only under granted writable paths", "fs.no_write_outside_workspace"),
		def(CapFSPathVirtualization, "sandbox sees logical roots (/workspace) rather than host paths", "fs.virtual_paths"),
		def(CapEnvScrub, "inherited env is allowlisted; credentials are not passed", "env.no_credentials"),
		def(CapProcessContainment, "all descendants die at teardown, including daemonized children", "proc.daemon_killed"),
		def(CapProcessCrossRunIsolation, "one run cannot signal another run's processes", "proc.cross_run_signal"),
		def(CapNetworkDirectNone, "no direct network, including loopback to host services, raw IPs and metadata endpoints", "net.none_loopback", "net.none_raw_ip", "net.none_metadata"),
		def(CapNetworkOutbound, "outbound connections allowed", "net.outbound_connect"),
		def(CapNetworkNoListen, "inbound listeners are blocked", "net.no_listen"),
		def(CapNetworkRestrictedEgress, "egress limited to granted hosts/CIDRs via a gateway behind default-deny networking", "net.restricted_allow", "net.restricted_deny"),
		def(CapNetworkControlledDNS, "name resolution is served by the runner and honours egress policy", "net.controlled_dns"),
		def(CapResourceCPU, "CPU capped", "res.cpu"),
		def(CapResourceMemory, "memory capped and OOM reported", "res.memory"),
		def(CapResourcePIDs, "process count capped", "res.pids"),
		def(CapResourceDisk, "writable disk usage capped", "res.disk"),
		def(CapResourceOutput, "captured output capped", "res.output"),
		def(CapResourceTimeout, "wall timeout: TERM, grace, KILL", "res.timeout"),
		def(CapDeadlineSurvivesRunner, "deadline is enforced even if the runner dies", "res.deadline_runner_crash"),
		def(CapOrphanRecovery, "sandboxes orphaned by a crash are reaped without touching live runners", "rt.orphan_recovery"),
		def(CapKernelIsolation, "syscalls served by a user-space kernel", "kernel.isolation"),
	} {
		m[d.ID] = d
	}
	return m
}()

// CapabilityIDs returns all registered IDs, sorted.
func CapabilityIDs() []CapabilityID {
	out := make([]CapabilityID, 0, len(Registry))
	for id := range Registry {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Required derives the capabilities a resolved spec needs: those implied by the
// policy plus explicit Requirements.
func Required(s SandboxSpec) []CapabilityID {
	set := map[CapabilityID]bool{}
	add := func(c ...CapabilityID) {
		for _, x := range c {
			set[x] = true
		}
	}
	add(CapEnvScrub, CapProcessContainment)
	if len(s.Filesystem.Deny) > 0 {
		add(CapFSDeny)
	}
	add(CapFSWriteScope)
	switch effEgress(s.Network.Egress) {
	case EgressNone:
		add(CapNetworkDirectNone)
	case EgressOutbound:
		add(CapNetworkOutbound)
	case EgressRestricted:
		add(CapNetworkRestrictedEgress)
	}
	if effDNS(s.Network.DNS, s.Network.Egress) == DNSControlled {
		add(CapNetworkControlledDNS)
	}
	if !listens(s.Network) {
		add(CapNetworkNoListen)
	}
	r := s.Resources
	if r.CPUs > 0 {
		add(CapResourceCPU)
	}
	if r.MemoryBytes > 0 {
		add(CapResourceMemory)
	}
	if r.PIDs > 0 {
		add(CapResourcePIDs)
	}
	if r.DiskBytes > 0 {
		add(CapResourceDisk)
	}
	if r.OutputBytes > 0 {
		add(CapResourceOutput)
	}
	if r.TimeoutSec > 0 {
		add(CapResourceTimeout)
	}
	add(s.Requirements...)
	out := make([]CapabilityID, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
