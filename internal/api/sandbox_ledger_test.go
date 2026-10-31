package api

import (
	"net/url"
	"testing"

	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
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

func TestAuditMax(t *testing.T) {
	for in, want := range map[string]int64{"": 1 << 30, "0": 0, " 0 ": 0, "512m": 512 << 20, "2g": 2 << 30, "4096": 4096} {
		if got, err := auditMax(in); err != nil || got != want {
			t.Errorf("auditMax(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"x", "-1", "1.5g"} {
		if _, err := auditMax(bad); err == nil {
			t.Errorf("auditMax(%q) accepted", bad)
		}
	}
}
