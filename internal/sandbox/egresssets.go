package sandbox

import (
	"slices"
	"strings"
)

// EgressSets are the named host groups an egress allowlist may use, e.g.
// AGENT_SANDBOX_EGRESS_ALLOW=@github,@npm,api.example.com: one place to keep
// a service's hosts, so a list names what an agent needs rather than how
// each service spreads over hosts. A wildcard grants one label under its
// domain (*.github.com: api.github.com, not github.com itself).
//
// A run's resolved spec carries the definitions of the sets its allowlist
// uses (NetworkPolicy.EgressSets), so its policy snapshot records what each
// meant for that run, whatever later releases change here.
var EgressSets = map[string][]string{
	"@anthropic":  {"api.anthropic.com"},
	"@openai":     {"api.openai.com", "chatgpt.com", "*.chatgpt.com"},
	"@deepseek":   {"api.deepseek.com"},
	"@github":     {"github.com", "*.github.com", "*.githubusercontent.com"},
	"@gitlab":     {"gitlab.com", "registry.gitlab.com"},
	"@npm":        {"registry.npmjs.org", "registry.yarnpkg.com"},
	"@pypi":       {"pypi.org", "files.pythonhosted.org"},
	"@go":         {"proxy.golang.org", "sum.golang.org"},
	"@cloudflare": {"api.cloudflare.com", "sparrow.cloudflare.com"}, // wrangler's API and its metrics
}

// SetNames lists the sets, sorted, for messages.
func SetNames() string {
	var names []string
	for n := range EgressSets {
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// UsedSets returns the definitions of the sets allow names (nil when it
// names none). A name with no definition is left out; the matcher reports it.
func UsedSets(allow []string) map[string][]string {
	var out map[string][]string
	for _, a := range allow {
		if !strings.HasPrefix(a, "@") {
			continue
		}
		if hosts, ok := EgressSets[strings.ToLower(a)]; ok {
			if out == nil {
				out = map[string][]string{}
			}
			out[strings.ToLower(a)] = slices.Clone(hosts)
		}
	}
	return out
}
