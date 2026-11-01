package sandbox

import (
	"slices"
	"strings"
	"testing"
)

// Every set is a list of plain host patterns under an @name the matcher
// accepts, and grants what its name says.
func TestEgressSetsAreValid(t *testing.T) {
	for name, hosts := range EgressSets {
		if !strings.HasPrefix(name, "@") || name != strings.ToLower(name) || len(hosts) == 0 {
			t.Errorf("set %q %v", name, hosts)
		}
		if _, err := NewHostMatcher(hosts, nil); err != nil {
			t.Errorf("set %s: %v", name, err)
		}
	}
	m, err := NewHostMatcher([]string{"@github", "@npm", "api.example.com"}, EgressSets)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"github.com": true, "api.github.com": true, "raw.githubusercontent.com": true, "registry.npmjs.org": true,
		"api.example.com": true, "gitlab.com": false, "evil.com": false, "a.b.github.com": false,
	} {
		if m.Match(host) != want {
			t.Errorf("%s: granted %v, want %v", host, !want, want)
		}
	}
}

func TestUsedSets(t *testing.T) {
	got := UsedSets([]string{"@github", "github.com", "@NPM", "@nope"})
	if len(got) != 2 || !slices.Equal(got["@github"], EgressSets["@github"]) || !slices.Equal(got["@npm"], EgressSets["@npm"]) {
		t.Errorf("UsedSets: %v", got)
	}
	if got := UsedSets([]string{"*"}); got != nil {
		t.Errorf("no sets used: %v", got)
	}
	got["@github"][0] = "changed"
	if EgressSets["@github"][0] == "changed" {
		t.Error("UsedSets hands out the table itself")
	}
}
