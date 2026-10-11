package runtime

import (
	"fmt"
	"strings"
	"testing"
)

func TestEgressLog(t *testing.T) {
	var events []string
	l := newEgressLog(func(kind, host string) { events = append(events, kind+" "+host) })
	for range 3 {
		l.record(true, "github.com")
	}
	l.record(false, "169.254.169.254")
	l.record(false, "169.254.169.254")
	l.record(true, "api.example.com")
	l.record(false, "evil|`host` \n")

	// One event per host and decision, not per connection.
	want := []string{"sandbox.egress_allowed github.com", "sandbox.egress_denied 169.254.169.254",
		"sandbox.egress_allowed api.example.com", "sandbox.egress_denied evil??host???"}
	if strings.Join(events, ";") != strings.Join(want, ";") {
		t.Errorf("events %q\nwant   %q", events, want)
	}
	got := l.summary()
	// Refused first, then by name; counts kept.
	if len(got) != 4 || got[0].Host != "169.254.169.254" || got[0].Denied != 2 || got[1].Host != "evil??host???" ||
		got[2].Host != "api.example.com" || got[3].Host != "github.com" || got[3].Allowed != 3 {
		t.Errorf("summary %+v", got)
	}

	// Bounded: the sandbox picks the names.
	b := newEgressLog(nil)
	for i := range maxEgressHosts + 7 {
		b.record(true, fmt.Sprintf("h%d.test", i))
	}
	s := b.summary()
	last := s[len(s)-1]
	if len(s) != maxEgressHosts+1 || last.Denied != 7 || !strings.Contains(last.Host, "more hosts") {
		t.Errorf("cap: %d entries, last %+v", len(s), last)
	}
	if (*egressLog)(nil).summary() != nil {
		t.Error("nil log")
	}
}
