package sandbox

import (
	"sort"

	"github.com/tendant/agentbox/pkg/agentbox/capability"
)

// The capability vocabulary lives in agentbox's capability package, shared
// with AgentGate; these aliases keep the sandbox package's names.
type (
	CapabilityID  = capability.ID
	CapabilityDef = capability.Def
	Gap           = capability.Gap
	CheckResult   = capability.CheckResult
)

const (
	CapFSDeny                   = capability.FSDeny
	CapFSWriteScope             = capability.FSWriteScope
	CapFSReadScope              = capability.FSReadScope
	CapFSPathVirtualization     = capability.FSPathVirtualization
	CapEnvScrub                 = capability.EnvScrub
	CapProcessContainment       = capability.ProcessContainment
	CapProcessCrossRunIsolation = capability.ProcessCrossRunIsolation
	CapNetworkDirectNone        = capability.NetworkDirectNone
	CapNetworkOutbound          = capability.NetworkOutbound
	CapNetworkNoListen          = capability.NetworkNoListen
	CapNetworkRestrictedEgress  = capability.NetworkRestrictedEgress
	CapNetworkControlledDNS     = capability.NetworkControlledDNS
	CapResourceCPU              = capability.ResourceCPU
	CapResourceMemory           = capability.ResourceMemory
	CapResourcePIDs             = capability.ResourcePIDs
	CapResourceDisk             = capability.ResourceDisk
	CapResourceOutput           = capability.ResourceOutput
	CapResourceTimeout          = capability.ResourceTimeout
	CapDeadlineSurvivesRunner   = capability.DeadlineSurvivesRunner
	CapOrphanRecovery           = capability.OrphanRecovery
	CapKernelIsolation          = capability.KernelIsolation
)

// RegistryVersion bumps whenever a capability's definition changes.
const RegistryVersion = capability.RegistryVersion

// Registry is the one source of capability definitions.
var Registry = capability.Registry

// CapabilityIDs returns all registered IDs, sorted.
func CapabilityIDs() []CapabilityID { return capability.IDs() }

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
	if s.Filesystem.ScopedReads {
		add(CapFSReadScope)
	}
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
