package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func base() SandboxSpec {
	return SandboxSpec{
		Version: SpecVersion,
		Filesystem: FilesystemPolicy{
			Grants: []PathGrant{{RootWorkspace, ReadWrite}, {RootTmp, ReadWrite}, {RootHome, ReadOnly}},
			Deny:   []string{RootHome + "/.ssh"},
		},
		Network:   NetworkPolicy{Egress: EgressRestricted, EgressAllow: []string{"*.github.com", "10.0.0.0/8", "@vcs"}},
		Resources: ResourcePolicy{MemoryBytes: 2 << 30, TimeoutSec: 600},
		Secrets:   []SecretGrant{{Name: "gh", Delivery: DeliverFD}},
	}
}

func TestParseSpecStrict(t *testing.T) {
	for name, in := range map[string]string{
		"unknown field":        `{"version":1,"bogus":1}`,
		"bad version":          `{"version":2}`,
		"no version":           `{}`,
		"trailing":             `{"version":1} {}`,
		"outside root":         `{"version":1,"filesystem":{"grants":[{"path":"/etc","access":"ro"}]}}`,
		"dotdot":               `{"version":1,"filesystem":{"deny":["/workspace/../etc"]}}`,
		"bad wildcard":         `{"version":1,"network":{"egress":"restricted","egress_allow":["*.*.com"]}}`,
		"allow w/o restricted": `{"version":1,"network":{"egress":"none","egress_allow":["a.com"]}}`,
		"unknown cap":          `{"version":1,"requirements":["fs.magic"]}`,
		"dup secret":           `{"version":1,"secrets":[{"name":"a","delivery":"fd"},{"name":"a","delivery":"fd"}]}`,
	} {
		if _, err := ParseSpec([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := ParseSpec([]byte(`{"version":1,"network":{"egress":"none"}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestHostSubset(t *testing.T) {
	cases := []struct {
		c, p string
		want bool
	}{
		{"10.1.0.0/16", "10.0.0.0/8", true},
		{"10.0.0.0/8", "10.1.0.0/16", false},
		{"10.2.3.4", "10.0.0.0/8", true},
		{"::1", "10.0.0.0/8", false},
		{"api.github.com", "*.github.com", true},
		{"a.b.github.com", "*.github.com", false}, // single label only
		{"github.com", "*.github.com", false},
		{"*.github.com", "*.github.com", true},
		{"*.api.github.com", "*.github.com", false},
		{"API.GitHub.com.", "*.github.com", true},
		{"evilgithub.com", "*.github.com", false},
		{"@vcs", "@vcs", true},
		{"github.com", "@vcs", false},
		{"@vcs", "github.com", false},
	}
	for _, c := range cases {
		got, _ := hostsSubset([]string{c.c}, []string{c.p})
		if got != c.want {
			t.Errorf("%s ⊆ %s = %v, want %v", c.c, c.p, got, c.want)
		}
	}
}

func TestResolveTightens(t *testing.T) {
	listenOff := false
	run := SandboxSpec{
		Version:      SpecVersion,
		Filesystem:   FilesystemPolicy{Grants: []PathGrant{{RootWorkspace + "/sub", ReadOnly}}, Deny: []string{RootWorkspace + "/.git"}},
		Network:      NetworkPolicy{EgressAllow: []string{"api.github.com", "10.1.0.0/16"}, Listen: &listenOff},
		Resources:    ResourcePolicy{MemoryBytes: 1 << 30},
		Requirements: []CapabilityID{CapKernelIsolation},
	}
	got, err := Resolve(Layer{LayerSystem, base()}, Layer{LayerRun, run})
	if err != nil {
		t.Fatal(err)
	}
	if got.Resources.MemoryBytes != 1<<30 || got.Resources.TimeoutSec != 600 {
		t.Errorf("resources: %+v", got.Resources)
	}
	if got.Network.Egress != EgressRestricted || got.Network.DNS != DNSControlled {
		t.Errorf("network: %+v", got.Network)
	}
	if len(got.Filesystem.Deny) != 2 || len(got.Secrets) != 1 {
		t.Errorf("deny/secrets: %+v %+v", got.Filesystem.Deny, got.Secrets)
	}
	if got.Filesystem.FSAccess(RootWorkspace+"/sub/x", true) {
		t.Error("ro grant must not allow write")
	}
	if got.Filesystem.FSAccess(RootWorkspace+"/.git/config", false) {
		t.Error("deny must win")
	}
}

func TestResolveRejectsWidening(t *testing.T) {
	yes := true
	cases := map[string]SandboxSpec{
		"egress outbound":  {Version: 1, Network: NetworkPolicy{Egress: EgressOutbound}},
		"host not covered": {Version: 1, Network: NetworkPolicy{EgressAllow: []string{"evil.com"}}},
		"broader cidr":     {Version: 1, Network: NetworkPolicy{EgressAllow: []string{"0.0.0.0/0"}}},
		"dns system":       {Version: 1, Network: NetworkPolicy{DNS: DNSSystem}},
		"listen":           {Version: 1, Network: NetworkPolicy{Listen: &yes}},
		"more memory":      {Version: 1, Resources: ResourcePolicy{MemoryBytes: 4 << 30}},
		"grant outside":    {Version: 1, Filesystem: FilesystemPolicy{Grants: []PathGrant{{RootHome, ReadWrite}}}},
		"new secret":       {Version: 1, Secrets: []SecretGrant{{Name: "aws", Delivery: DeliverEnv, Target: "AWS"}}},
		"secret changed":   {Version: 1, Secrets: []SecretGrant{{Name: "gh", Delivery: DeliverEnv, Target: "GH"}}},
	}
	for name, l := range cases {
		_, err := Resolve(Layer{LayerSystem, base()}, Layer{LayerRun, l})
		if err == nil || !strings.Contains(err.Error(), "run") {
			t.Errorf("%s: expected widening error naming layer, got %v", name, err)
		}
	}
}

func TestResolveDefaultsFailClosed(t *testing.T) {
	got, err := Resolve(Layer{LayerSystem, SandboxSpec{Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Network.Egress != EgressNone || got.Network.DNS != DNSNone {
		t.Errorf("%+v", got.Network)
	}
	// Child cannot widen from the none default either.
	_, err = Resolve(Layer{LayerSystem, SandboxSpec{Version: 1}},
		Layer{LayerBot, SandboxSpec{Version: 1, Network: NetworkPolicy{Egress: EgressOutbound}}})
	if err == nil {
		t.Error("expected error")
	}
}

func TestCheckStrictAndPermissive(t *testing.T) {
	spec, err := Resolve(Layer{LayerSystem, base()})
	if err != nil {
		t.Fatal(err)
	}
	weak := ManifestBackend{Manifest{Backend: "seatbelt", RegistryVersion: RegistryVersion,
		Capabilities: []CapabilityID{CapEnvScrub, CapProcessContainment, CapFSDeny, CapFSWriteScope, CapNetworkNoListen, CapResourceTimeout}}}
	res, _ := weak.Check(context.Background(), spec)
	var ids []string
	for _, g := range res.Gaps {
		ids = append(ids, string(g.Capability))
	}
	for _, want := range []CapabilityID{CapNetworkRestrictedEgress, CapNetworkControlledDNS, CapResourceMemory} {
		if !strings.Contains(strings.Join(ids, ","), string(want)) {
			t.Errorf("missing gap %s in %v", want, ids)
		}
	}
	var ge *GapError
	if err := Admit(ModeStrict, res); !errors.As(err, &ge) {
		t.Errorf("strict must reject: %v", err)
	}
	if err := Admit(ModePermissive, res); err != nil {
		t.Errorf("permissive must admit: %v", err)
	}
	full := ManifestBackend{Manifest{Backend: "prod", RegistryVersion: RegistryVersion, Capabilities: CapabilityIDs()}}
	res, _ = full.Check(context.Background(), spec)
	if err := Admit(ModeStrict, res); err != nil {
		t.Errorf("full backend: %v", err)
	}
	if _, err := full.Prepare(context.Background(), spec); err == nil {
		t.Error("manifest backend must not prepare")
	}
}
