package api

import (
	"net/url"
	"testing"

	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
)

func TestLedgerChannels(t *testing.T) {
	ch := func(u string) *sandboxrt.ModelChannel {
		p, _ := url.Parse(u)
		return &sandboxrt.ModelChannel{Upstream: p}
	}
	chs := map[string]*sandboxrt.ModelChannel{
		"anthropic": ch("https://Ledger.internal:8443"),
		"openai":    ch("https://api.openai.com"),
		"plain":     ch("http://ledger.internal:8443"), // same host, other scheme
	}
	ledgerChannels(chs, "https://ledger.internal:8443/")
	if !chs["anthropic"].Ledger || chs["openai"].Ledger || chs["plain"].Ledger {
		t.Errorf("ledger marks: anthropic %v openai %v plain %v", chs["anthropic"].Ledger, chs["openai"].Ledger, chs["plain"].Ledger)
	}
	for _, bad := range []string{"", "not a url", "ledger.internal"} {
		chs := map[string]*sandboxrt.ModelChannel{"anthropic": ch("https://ledger.internal")}
		if ledgerChannels(chs, bad); chs["anthropic"].Ledger {
			t.Errorf("ledger URL %q marked a channel", bad)
		}
	}
}

// AGENT_SANDBOX_METERING overrides a channel's default metering; an entry
// with a bad mode or for a provider without a channel changes nothing.
func TestMeteredChannels(t *testing.T) {
	chs := map[string]*sandboxrt.ModelChannel{
		"anthropic": {},
		"deepseek":  {},
		"openai":    {Ledger: true},
	}
	meteredChannels(chs, []string{"deepseek=none", " anthropic=proxied", "gemini=none", "openai"})
	if got := chs["deepseek"].MeteringMode(); got != audit.MeteringNone {
		t.Errorf("deepseek: %s, want none as declared", got)
	}
	if got := chs["anthropic"].MeteringMode(); got != audit.MeteringBroker {
		t.Errorf("anthropic: %s, want the default broker (bad mode ignored)", got)
	}
	if got := chs["openai"].MeteringMode(); got != audit.MeteringLedger {
		t.Errorf("openai: %s, want the default ledger (an entry without a mode is ignored)", got)
	}
	if _, ok := chs["gemini"]; ok {
		t.Error("an entry for a provider without a channel added one")
	}
}

func TestAuditOptions(t *testing.T) {
	for _, c := range []struct {
		segment, keep string
		want          audit.DirOptions
	}{
		{"", "", audit.DirOptions{SegmentBytes: 10 << 20, Keep: 10}},
		{"512m", "0", audit.DirOptions{SegmentBytes: 512 << 20, Keep: 0}},
		{" 4096 ", " 3 ", audit.DirOptions{SegmentBytes: 4096, Keep: 3}},
	} {
		if got, err := auditOptions(c.segment, c.keep); err != nil || got != c.want {
			t.Errorf("auditOptions(%q, %q) = %+v, %v; want %+v", c.segment, c.keep, got, err, c.want)
		}
	}
	for _, bad := range [][2]string{{"0", ""}, {"x", ""}, {"1.5g", ""}, {"", "-1"}, {"", "x"}} {
		if _, err := auditOptions(bad[0], bad[1]); err == nil {
			t.Errorf("auditOptions(%q, %q) accepted", bad[0], bad[1])
		}
	}
}
