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

// AGENT_SANDBOX_MODEL_DOWNGRADE allows downgrades only on ledger channels;
// other names change nothing.
func TestDowngradeChannels(t *testing.T) {
	chs := map[string]*sandboxrt.ModelChannel{
		"anthropic": {Ledger: true},
		"openai":    {Ledger: true},
		"deepseek":  {},
	}
	downgradeChannels(chs, []string{" anthropic", "deepseek", "gemini", ""})
	if !chs["anthropic"].AllowDowngrade || chs["openai"].AllowDowngrade || chs["deepseek"].AllowDowngrade {
		t.Errorf("allow: anthropic %v openai %v deepseek %v", chs["anthropic"].AllowDowngrade, chs["openai"].AllowDowngrade, chs["deepseek"].AllowDowngrade)
	}
	if _, ok := chs["gemini"]; ok {
		t.Error("an entry for a provider without a channel added one")
	}
}

// An OpenAI-compatible provider's base URL keeps its path: pi sends
// /chat/completions, so a base written with /v1 must reach /v1/... (an
// upstream serving only /v1, like vLLM or ledger-gate before it took both,
// answered 404 when the /v1 was dropped).
func TestCompatChannelKeepsBasePath(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	for base, wantPath := range map[string]string{
		"http://gateway.internal/v1":  "/v1",
		"http://gateway.internal/v1/": "/v1",
		"https://api.deepseek.com":    "",
	} {
		t.Setenv("DEEPSEEK_BASE_URL", base)
		ch := withCompatChannels(map[string]*sandboxrt.ModelChannel{})["deepseek"]
		if ch == nil || ch.Upstream.Path != wantPath {
			t.Errorf("%s: channel %+v, want upstream path %q", base, ch, wantPath)
		}
	}
	// OpenAI's channel still drops it: codex and pi send /v1/... themselves.
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	if u := upstreamURL("OPENAI_BASE_URL", ""); u == nil || u.Path != "" {
		t.Errorf("openai upstream %v, want no /v1", u)
	}
}
