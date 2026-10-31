package sandbox

import "github.com/tendant/agentbox/pkg/agentbox/egress"

// Host patterns live in agentbox's egress package, beside the gateway that
// enforces them; these keep the sandbox package's names.

// HostMatcher decides whether a destination is granted by egress patterns.
type HostMatcher = egress.HostMatcher

// NewHostMatcher parses patterns; sets maps "@name" to its members.
func NewHostMatcher(patterns []string, sets map[string][]string) (*HostMatcher, error) {
	return egress.NewHostMatcher(patterns, sets)
}

func validHostPattern(s string) error { return egress.ValidateHostPattern(s) }

func hostsSubset(child, parent []string) (bool, string) { return egress.HostsSubset(child, parent) }
