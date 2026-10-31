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
