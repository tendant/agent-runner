package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
	"github.com/tendant/agentbox/pkg/agentbox/broker"
	"github.com/tendant/agentbox/pkg/agentbox/pricing"
)

// auditRT is newRT with an audit record at path; read returns what it holds.
func auditRT(t *testing.T, mode sandbox.Mode, gaps []sandbox.Gap, cfg Config) (rt *Runtime, read func() []audit.Event, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "audit.jsonl")
	rec, err := audit.OpenFileRecorder(path, "test-host", audit.FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	got := &sandbox.ProcSpec{}
	cfg.Mode, cfg.StateDir, cfg.Instance, cfg.LeaseTTL, cfg.Audit = mode, t.TempDir(), "test", time.Second, rec
	cfg.NewBackend = func(map[string]string) sandbox.Backend { return fakeBackend{gaps, got} }
	rt, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	read = func() []audit.Event {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var out []audit.Event
		d := audit.NewDecoder(f, 0)
		for {
			e, err := d.Next()
			if errors.Is(err, io.EOF) {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
	}
	return rt, read, path
}

func types(evs []audit.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestAuditRecordsARun(t *testing.T) {
	rt, read, _ := auditRT(t, sandbox.ModePermissive, []sandbox.Gap{{Capability: sandbox.CapResourceMemory, Reason: "no cgroup"}}, Config{Agent: "travel"})
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Project: "web-app", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run.egress.record(egressAllowed, "api.github.com")
	run.egress.record(egressAllowed, "api.github.com") // every decision, not just the first per host
	run.egress.record(egressDenied, "169.254.169.254")
	run.Finish()

	evs := read()
	want := []string{audit.TypePolicySnapshot, audit.TypeSandboxCreated, audit.TypeNetworkAllowed, audit.TypeNetworkAllowed,
		audit.TypeNetworkBlocked, audit.TypeSandboxClosed}
	if got := types(evs); len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	policy := evs[0].PolicyID
	for i, e := range evs {
		if e.Type != want[i] || e.RunID != "r1" || e.PolicyID != policy || e.SourceSequence != uint64(i+1) {
			t.Errorf("event %d: %+v", i, e)
		}
	}

	// The snapshot is the resolved policy and the backend's answer, and its
	// digest is recomputable from the record.
	if d, _ := audit.PolicyDigest(evs[0].Detail); d != policy {
		t.Errorf("policy id %s, digest of the snapshot %s", policy, d)
	}
	var snap struct {
		Backend, Mode string
		Spec          sandbox.SandboxSpec
		Check         sandbox.CheckResult
	}
	json.Unmarshal(evs[0].Detail, &snap)
	if snap.Backend != "fake" || snap.Mode != "permissive" || snap.Spec.Network.Egress != sandbox.EgressRestricted ||
		len(snap.Check.Gaps) != 1 || snap.Check.Gaps[0].Capability != sandbox.CapResourceMemory {
		t.Errorf("snapshot %+v", snap)
	}
	var blocked audit.NetworkDetail
	json.Unmarshal(evs[4].Detail, &blocked)
	if blocked.Destination != "169.254.169.254" {
		t.Errorf("blocked detail %s", evs[4].Detail)
	}
	var created audit.LifecycleDetail
	json.Unmarshal(evs[1].Detail, &created)
	if created.Backend != "fake" || created.ThreadID != "t1" || created.Reason != "unenforced: resource.memory" ||
		created.Agent != "travel" || created.Project != "web-app" {
		t.Errorf("created detail %s", evs[1].Detail)
	}
	var closed audit.LifecycleDetail
	json.Unmarshal(evs[len(evs)-1].Detail, &closed)
	if closed.Agent != "" || closed.Project != "" {
		t.Errorf("whose run it is belongs on the start, not every lifecycle event: %s", evs[len(evs)-1].Detail)
	}
	if len(rt.auditRuns) != 0 {
		t.Errorf("finished run still followed: %v", rt.auditRuns)
	}
}

// A strict run refused for its gaps is on record with the policy it was
// refused under.
func TestAuditRecordsARejection(t *testing.T) {
	rt, read, _ := auditRT(t, sandbox.ModeStrict, []sandbox.Gap{{Capability: sandbox.CapResourceMemory}}, Config{})
	if _, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()}); err == nil {
		t.Fatal("strict run with gaps started")
	}
	evs := read()
	if len(evs) != 2 || evs[0].Type != audit.TypePolicySnapshot || evs[1].Type != audit.TypeSandboxRejected ||
		evs[1].PolicyID == "" || evs[1].PolicyID != evs[0].PolicyID {
		t.Fatalf("events %+v", evs)
	}
	if len(rt.auditRuns) != 0 {
		t.Errorf("refused run still followed: %v", rt.auditRuns)
	}

	// A policy that fails to resolve is refused before any check: no
	// snapshot, but still on record.
	wide := sandbox.SandboxSpec{Version: 1, Filesystem: sandbox.FilesystemPolicy{Grants: []sandbox.PathGrant{{Path: "/etc", Access: sandbox.ReadWrite}}}}
	if _, err := rt.Begin(context.Background(), BeginReq{RunID: "r2", Workspace: t.TempDir(), Layers: []sandbox.Layer{{Name: sandbox.LayerRun, Spec: wide}}}); err == nil {
		t.Fatal("widening policy started")
	}
	evs = read()
	if last := evs[len(evs)-1]; last.Type != audit.TypeSandboxRejected || last.RunID != "r2" || last.PolicyID != "" {
		t.Errorf("unresolvable policy: %+v", last)
	}
}

// A lost thread lease names no run; the record charges it to the thread's
// started runs.
func TestAuditEnforcementLostOnThread(t *testing.T) {
	rt, read, _ := auditRT(t, sandbox.ModePermissive, nil, Config{})
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Finish()
	rt.emit(Event{Kind: "sandbox.enforcement_lost", ThreadID: "t1", Detail: "thread lease lost"})
	rt.emit(Event{Kind: "sandbox.enforcement_lost", ThreadID: "other", Detail: "thread lease lost"})
	evs := read()
	last := evs[len(evs)-1]
	if last.Type != audit.TypeEnforcementLost || last.RunID != "r1" || last.PolicyID != evs[0].PolicyID || len(evs) != 3 {
		t.Errorf("events %v, last %+v", types(evs), last)
	}
}

// Every model-channel request is on record, forwarded or refused, with the
// provider and never the credential.
func TestAuditRecordsModelRequests(t *testing.T) {
	anthropic, _ := upstream(t, "Authorization")
	rt, read, path := auditRT(t, sandbox.ModePermissive, nil, Config{Models: map[string]*ModelChannel{
		ProviderAnthropic: {Upstream: anthropic, AuthHeader: "x-api-key", AuthValue: "real-key", Paths: AnthropicPaths},
	}})
	got := &sandbox.ProcSpec{}
	rt.cfg.NewBackend = func(map[string]string) sandbox.Backend { return fakeBackend{nil, got} }
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	env := launch(t, run, got, "claude")
	base := fromHost(run, env["ANTHROPIC_BASE_URL"])
	if code := post(t, base+"/v1/messages", env["ANTHROPIC_API_KEY"]+env["ANTHROPIC_AUTH_TOKEN"]); code != 200 {
		t.Fatalf("forwarded request: %d", code)
	}
	post(t, base+"/v1/messages", "forged")
	post(t, base+"/api/oauth/profile?token=secret", env["ANTHROPIC_API_KEY"]+env["ANTHROPIC_AUTH_TOKEN"])
	run.Finish()

	var model []audit.Event
	for _, e := range read() {
		if e.Type == audit.TypeModelForwarded || e.Type == audit.TypeModelDenied {
			model = append(model, e)
		}
	}
	if len(model) != 3 || model[0].Type != audit.TypeModelForwarded || model[1].Type != audit.TypeModelDenied || model[2].Type != audit.TypeModelDenied {
		t.Fatalf("model events %v", types(model))
	}
	// A refusal says why, and the path it asked for (never its query).
	for i, want := range map[int][2]string{1: {"token", "/v1/messages"}, 2: {"path", "/api/oauth/profile"}} {
		var d audit.ModelDetail
		json.Unmarshal(model[i].Detail, &d)
		if d.Reason != want[0] || d.Path != want[1] {
			t.Errorf("refusal %d: reason %q path %q, want %v", i, d.Reason, d.Path, want)
		}
	}
	for _, e := range model {
		var d audit.ModelDetail
		json.Unmarshal(e.Detail, &d)
		if d.Provider != ProviderAnthropic || e.PolicyID == "" {
			t.Errorf("model event %+v", e)
		}
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "real-key") || strings.Contains(string(raw), "forged") || strings.Contains(string(raw), "secret") {
		t.Error("a credential reached the audit record")
	}
}

// A ledger-gate channel's requests carry the run's id, whatever the sandbox
// sent; a direct provider channel's never get one from the proxy.
func TestLedgerChannelCarriesRunID(t *testing.T) {
	ledger, ledgerGot := upstream(t, RunHeader)
	direct, directGot := upstream(t, RunHeader)
	t.Setenv("OPENAI_API_KEY", "")
	got := &sandbox.ProcSpec{}
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Second,
		Models: map[string]*ModelChannel{
			ProviderAnthropic: {Upstream: ledger, AuthHeader: "x-api-key", AuthValue: "ledger-key", Paths: AnthropicPaths, Ledger: true},
			ProviderOpenAI:    {Upstream: direct, AuthHeader: "Authorization", AuthValue: "Bearer real-key", Paths: OpenAIPaths},
		},
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{nil, got} }})
	if err != nil {
		t.Fatal(err)
	}
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Finish()

	anthropic := run.model.proxies[ProviderAnthropic]
	req, _ := http.NewRequest("POST", anthropic.BaseURL()+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", anthropic.Token())
	req.Header.Set(RunHeader, "someone-elses-run")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("ledger channel: %v %v", err, resp)
	}
	resp.Body.Close()
	if *ledgerGot != "r1" {
		t.Errorf("ledger got %s %q, want r1", RunHeader, *ledgerGot)
	}

	openai := run.model.proxies[ProviderOpenAI]
	if code := post(t, openai.BaseURL()+"/v1/chat/completions", openai.Token()); code != 200 {
		t.Fatalf("direct channel: %d", code)
	}
	if *directGot != "" {
		t.Errorf("a direct provider got %s %q", RunHeader, *directGot)
	}
	// Not even the sandbox's own: a channel not marked as a ledger (a
	// misconfigured AGENT_SANDBOX_LEDGER_URL) must not pass a forgery on.
	req, _ = http.NewRequest("POST", openai.BaseURL()+"/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+openai.Token())
	req.Header.Set(RunHeader, "someone-elses-run")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if *directGot != "" {
		t.Errorf("a sandbox-sent %s reached an unmarked channel: %q", RunHeader, *directGot)
	}
}

// usageUpstream answers like Anthropic's Messages API, with usage, and keeps
// the request id header each call carried.
func usageUpstream(t *testing.T) (*url.URL, *[]string) {
	t.Helper()
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get(RequestHeader))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"claude-x","usage":{"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":500}}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u, &ids
}

func modelEvents(evs []audit.Event) (snap audit.PolicySnapshot, fwd []audit.ModelDetail, use []audit.UsageDetail) {
	for _, e := range evs {
		switch e.Type {
		case audit.TypePolicySnapshot:
			json.Unmarshal(e.Detail, &snap)
		case audit.TypeModelForwarded:
			var d audit.ModelDetail
			json.Unmarshal(e.Detail, &d)
			fwd = append(fwd, d)
		case audit.TypeModelUsage:
			var d audit.UsageDetail
			json.Unmarshal(e.Detail, &d)
			use = append(use, d)
		}
	}
	return snap, fwd, use
}

// A call no ledger sees is metered by the broker: the snapshot declares it,
// model_forwarded names the call, and its usage follows under the same id,
// billed as the channel is paid (a subscription token here).
func TestBrokerMeteredCallRecordsItsUsage(t *testing.T) {
	up, ids := usageUpstream(t)
	rt, read, _ := auditRT(t, sandbox.ModePermissive, nil, Config{Models: map[string]*ModelChannel{
		ProviderAnthropic: {Upstream: up, AuthHeader: "Authorization", AuthValue: "Bearer sub-token", Paths: AnthropicPaths, OAuth: true},
	}})
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := run.model.proxies[ProviderAnthropic]
	if code := post(t, p.BaseURL()+"/v1/messages", p.Token()); code != 200 {
		t.Fatalf("forwarded request: %d", code)
	}
	run.Finish()

	snap, fwd, use := modelEvents(read())
	if snap.Metering[ProviderAnthropic] != audit.MeteringBroker {
		t.Errorf("snapshot metering %v", snap.Metering)
	}
	if len(fwd) != 1 || fwd[0].Metering != audit.MeteringBroker || !strings.HasPrefix(fwd[0].RequestID, "req_") {
		t.Fatalf("model_forwarded %+v", fwd)
	}
	want := audit.UsageDetail{RequestID: fwd[0].RequestID, Provider: ProviderAnthropic, Model: "claude-x",
		InputTokens: 12, OutputTokens: 34, CacheReadTokens: 500, Billing: audit.BillingSubscription}
	if len(use) != 1 || use[0] != want {
		t.Errorf("model_usage %+v, want %+v", use, want)
	}
	if (*ids)[0] != "" {
		t.Errorf("a provider got the broker's request id %q", (*ids)[0])
	}
}

// With a model budget, a run's broker-metered calls are charged at list
// price, a model the table lacks at its provider's highest rates. Once spent,
// the proxy refuses the next call, on record with why; the snapshot names
// the budget and the prices.
func TestModelBudgetRefusesOnceSpent(t *testing.T) {
	up, _ := usageUpstream(t)
	rt, read, _ := auditRT(t, sandbox.ModePermissive, nil, Config{ModelBudget: 2000, Models: map[string]*ModelChannel{
		ProviderAnthropic: {Upstream: up, AuthHeader: "Authorization", AuthValue: "Bearer sub-token", Paths: AnthropicPaths, OAuth: true},
	}})
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := run.model.proxies[ProviderAnthropic]
	if code := post(t, p.BaseURL()+"/v1/messages", p.Token()); code != 200 {
		t.Fatalf("first call: %d", code)
	}
	// claude-x is unpriced: 12 input, 34 output and 500 cache-read tokens at
	// Anthropic's ceiling come to over 2000 millionths. Its usage is read as
	// the proxy finishes the response, which can trail the client a little.
	code := 0
	for i := 0; i < 100 && code != 429; i++ {
		if code = post(t, p.BaseURL()+"/v1/messages", p.Token()); code != 429 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	run.Finish()
	if code != 429 {
		t.Fatalf("over budget: %d", code)
	}
	evs := read()
	snap, _, _ := modelEvents(evs)
	if snap.ModelBudget == nil || snap.ModelBudget.USDMicros != 2000 || snap.ModelBudget.Prices != pricing.Default().Version {
		t.Errorf("snapshot budget %+v", snap.ModelBudget)
	}
	var hit []audit.ModelDetail
	for _, e := range evs {
		if e.Type == audit.TypeModelLimitHit {
			var d audit.ModelDetail
			json.Unmarshal(e.Detail, &d)
			hit = append(hit, d)
		}
	}
	if len(hit) != 1 || hit[0].Reason != broker.RefuseUsage || hit[0].Path != "/v1/messages" {
		t.Errorf("model_limit_hit %+v", hit)
	}
}

// No budget: none in the snapshot, nothing refused.
func TestNoModelBudget(t *testing.T) {
	if BudgetMicros(0) != 0 || BudgetMicros(-1) != 0 || BudgetMicros(2.5) != 2_500_000 {
		t.Error("BudgetMicros")
	}
	var s *modelSpend
	s.charge(ProviderAnthropic, broker.Usage{OutputTokens: 1}) // a run without a budget charges nothing
}

// A ledger channel's call is metered by ledger-gate: it gets the call's id
// next to the run's, and the record expects the ledger row, not a usage
// event. AGENT_SANDBOX_METERING can declare another mode.
func TestLedgerMeteredCallCarriesItsID(t *testing.T) {
	up, ids := usageUpstream(t)
	direct, _ := usageUpstream(t)
	rt, read, _ := auditRT(t, sandbox.ModePermissive, nil, Config{Models: map[string]*ModelChannel{
		ProviderAnthropic: {Upstream: up, AuthHeader: "x-api-key", AuthValue: "ledger-key", Paths: AnthropicPaths, Ledger: true},
		ProviderOpenAI:    {Upstream: direct, AuthHeader: "Authorization", AuthValue: "Bearer k", Paths: OpenAIPaths, Metering: audit.MeteringNone},
	}})
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := run.model.proxies[ProviderAnthropic]
	req, _ := http.NewRequest("POST", p.BaseURL()+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", p.Token())
	req.Header.Set(RequestHeader, "req_forged")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	o := run.model.proxies[ProviderOpenAI]
	post(t, o.BaseURL()+"/v1/chat/completions", o.Token())
	run.Finish()

	snap, fwd, use := modelEvents(read())
	if snap.Metering[ProviderAnthropic] != audit.MeteringLedger || snap.Metering[ProviderOpenAI] != audit.MeteringNone {
		t.Errorf("snapshot metering %v", snap.Metering)
	}
	if len(fwd) != 2 || len(use) != 0 {
		t.Fatalf("forwarded %+v, usage %+v: neither ledger nor none calls get usage events", fwd, use)
	}
	byProvider := map[string]audit.ModelDetail{fwd[0].Provider: fwd[0], fwd[1].Provider: fwd[1]}
	if a := byProvider[ProviderAnthropic]; a.Metering != audit.MeteringLedger || (*ids)[0] != a.RequestID {
		t.Errorf("ledger got request id %q for call %+v", (*ids)[0], a)
	}
	if byProvider[ProviderOpenAI].Metering != audit.MeteringNone {
		t.Errorf("declared metering not on the call: %+v", byProvider[ProviderOpenAI])
	}
}

// A run's policy snapshot records what the @sets of its allowlist meant.
func TestAuditSnapshotDefinesEgressSets(t *testing.T) {
	rt, read, _ := auditRT(t, sandbox.ModePermissive, nil, Config{EgressAllow: []string{"@github", "api.example.com"}})
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run.Finish()
	var snap struct {
		Spec sandbox.SandboxSpec
	}
	json.Unmarshal(read()[0].Detail, &snap)
	n := snap.Spec.Network
	if !slices.Equal(n.EgressAllow, []string{"@github", "api.example.com"}) || len(n.EgressSets) != 1 ||
		!slices.Equal(n.EgressSets["@github"], sandbox.EgressSets["@github"]) {
		t.Errorf("snapshot network %+v", n)
	}
}

// A reported allowlist is on the snapshot, and a connection outside it is
// on record as allowed, with the reason that says what enforcing would do.
func TestAuditEgressReportOnly(t *testing.T) {
	rt, read, _ := auditRT(t, sandbox.ModePermissive, nil, Config{EgressAllow: []string{"@github"}, EgressReport: true})
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run.egress.record(egressAllowed, "api.github.com")
	run.egress.record(egressOutside, "evil.example")
	run.Finish()
	evs := read()
	var snap struct{ Spec sandbox.SandboxSpec }
	json.Unmarshal(evs[0].Detail, &snap)
	if !snap.Spec.Network.EgressReportOnly {
		t.Errorf("snapshot network %+v: report only not recorded", snap.Spec.Network)
	}
	var reasons []string
	for _, e := range evs {
		if e.Type == audit.TypeNetworkAllowed {
			var d audit.NetworkDetail
			json.Unmarshal(e.Detail, &d)
			reasons = append(reasons, d.Destination+":"+d.Reason)
		}
	}
	if !slices.Equal(reasons, []string{"api.github.com:", "evil.example:" + audit.ReasonOutsideAllowlist}) {
		t.Errorf("allowed connections %v", reasons)
	}
	// Enforcing (the default) says nothing of reporting.
	rt2, read2, _ := auditRT(t, sandbox.ModePermissive, nil, Config{EgressAllow: []string{"@github"}})
	run2, _ := rt2.Begin(context.Background(), BeginReq{RunID: "r2", ThreadID: "t2", Workspace: t.TempDir()})
	run2.Finish()
	var snap2 struct{ Spec sandbox.SandboxSpec }
	json.Unmarshal(read2()[0].Detail, &snap2)
	if snap2.Spec.Network.EgressReportOnly {
		t.Error("an enforced allowlist recorded as report only")
	}
}
