package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agent-runner/agent-runner/internal/safefs"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
	"github.com/tendant/agentbox/pkg/agentbox/broker"
	"github.com/tendant/agentbox/pkg/agentbox/pricing"
)

// Providers with a model channel.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// CompatProvider is a provider with an OpenAI-compatible chat API, as pi
// names it among its built-ins. pi gets its proxy as that provider's baseUrl
// (models.json) and the run token in KeyEnv. Adding one is a row here.
type CompatProvider struct {
	Name     string // pi's provider name; the channel's key
	KeyEnv   string // the API key variable, on the host and for pi
	BaseEnv  string // overrides Upstream on the host
	Upstream string // default API base (pi's baseUrl: no /v1 for DeepSeek)
}

// CompatProviders are the OpenAI-compatible providers the runner proxies.
var CompatProviders = []CompatProvider{
	{Name: "deepseek", KeyEnv: "DEEPSEEK_API_KEY", BaseEnv: "DEEPSEEK_BASE_URL", Upstream: "https://api.deepseek.com"},
}

// CompatPaths are the chat API paths an OpenAI-compatible client uses, with
// and without a /v1 base.
var CompatPaths = []string{"/chat/completions", "/models", "/v1/chat/completions", "/v1/models"}

// ModelChannel is a provider credential that sandboxed agent CLIs reach the
// model with. The runtime starts a proxy (internal/sandbox/proxy) per run and
// provider that attaches it; the CLIs get only the proxy's URL and a per-run
// token, so the credential never enters the sandbox.
type ModelChannel struct {
	Upstream   *url.URL // e.g. https://api.anthropic.com, https://api.openai.com
	AuthHeader string   // "Authorization" ("Bearer ...") or "x-api-key"
	AuthValue  string
	Paths      []string // request paths the CLIs use; the proxy refuses others
	// OAuth marks a subscription token (`claude setup-token`) rather than an
	// API key: pi must then send its run token as an OAuth token.
	OAuth bool
	// Credential, instead of AuthHeader/AuthValue, gives the headers per
	// request: a login that refreshes (internal/sandbox/codexlogin).
	Credential func(context.Context) (http.Header, error)
	// ChatGPT marks an OpenAI channel holding a codex ChatGPT login: its
	// upstream is ChatGPT's codex backend, which only codex speaks.
	ChatGPT bool
	// Ledger marks an upstream that is ledger-gate: the proxy tags every
	// request with the run's id (RunHeader) and its own request id
	// (RequestHeader), which ledger-gate records for the audit and never
	// passes on to the provider.
	Ledger bool
	// Metering is how the channel's calls are metered, declared in each
	// run's policy snapshot (audit.MeteringLedger, MeteringBroker,
	// MeteringNone). Empty: see MeteringMode.
	Metering string
}

// MeteringMode is the channel's metering: Metering when set, else ledger
// for a ledger-gate upstream and broker for any other, since the proxy reads
// every response it forwards.
func (ch *ModelChannel) MeteringMode() string {
	switch {
	case ch.Metering != "":
		return ch.Metering
	case ch.Ledger:
		return audit.MeteringLedger
	}
	return audit.MeteringBroker
}

// billing is how the channel is paid for: a subscription login (Claude's
// setup-token, a ChatGPT login) is flat-rate, a key per token.
func (ch *ModelChannel) billing() string {
	if ch.OAuth || ch.ChatGPT {
		return audit.BillingSubscription
	}
	return audit.BillingAPI
}

// usageFormat is how provider name's responses report usage: Anthropic's
// Messages API, or OpenAI's (the OpenAI-compatible providers too).
func usageFormat(name string) broker.UsageFormat {
	if name == ProviderAnthropic {
		return broker.UsageAnthropic
	}
	return broker.UsageOpenAI
}

// RunHeader carries a run's id to a ledger-gate upstream. The proxy
// sets it, replacing any the sandbox sent; ledger-gate trusts it only from
// keys marked to assert it.
const RunHeader = "Agentbox-Run-Id"

// RequestHeader carries the proxy's id for one call to a ledger-gate
// upstream, as RunHeader does the run's: the id on the call's
// model_forwarded event, so the two can be matched.
const RequestHeader = "Agentbox-Request-Id"

// Request paths per provider: the claude CLI checks /api/hello and posts
// messages (count_tokens is under it); codex and pi use the Responses and
// Chat Completions APIs.
var (
	AnthropicPaths = []string{"/v1/messages", "/api/hello"}
	OpenAIPaths    = []string{"/v1/responses", "/v1/chat/completions", "/v1/models"}
	// ChatGPTPaths: codex in ChatGPT mode posts responses (and
	// responses/compact) and lists models under this base.
	ChatGPTPaths = []string{chatgptBase + "/responses", chatgptBase + "/models"}
)

// chatgptBase is the path of ChatGPT's codex backend (https://chatgpt.com).
const chatgptBase = "/backend-api/codex"

// modelCredentialEnv are the host variables that would hand the sandbox a
// provider credential or route around the proxy; with a channel on they are
// never passed in, whatever AGENT_SANDBOX_ENV_ALLOW lists.
var modelCredentialEnv = map[string][]string{
	ProviderAnthropic: {"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_BASE_URL"},
	ProviderOpenAI:    {"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"},
}

func init() {
	for _, p := range CompatProviders {
		modelCredentialEnv[p.Name] = []string{p.KeyEnv, p.BaseEnv}
	}
}

// modelRun is a run's started channels: per provider, the proxy.
type modelRun struct {
	proxies map[string]*broker.Proxy
	chatgpt bool // the OpenAI channel is a ChatGPT login (codex only)
	// anthropicOAuth: the Anthropic channel is a subscription token, which
	// pi takes as ANTHROPIC_OAUTH_TOKEN (not an auth.json API key).
	anthropicOAuth bool
	sockDir        string            // Linux: the proxies' Unix sockets
	inSandbox      map[string]string // Linux: provider -> forwarder address
}

func (m *modelRun) close() {
	for _, p := range m.proxies {
		_ = p.Close()
	}
	if m.sockDir != "" {
		os.RemoveAll(m.sockDir)
	}
}

// startModels starts a proxy for each configured provider. The sandbox
// reaches them with no network: on Linux (gVisor, its own network stack)
// over Unix sockets mounted into it, on macOS (Seatbelt, the host's stack)
// at their loopback ports, which are all it may connect to.
func (r *Runtime) startModels(spec sandbox.SandboxSpec, runID, threadID string) (*modelRun, error) {
	if len(r.cfg.Models) == 0 {
		return nil, nil
	}
	m := &modelRun{proxies: map[string]*broker.Proxy{}}
	var spend *modelSpend
	if r.cfg.ModelBudget > 0 {
		spend = &modelSpend{budget: r.cfg.ModelBudget}
	}
	if modelSockets {
		// A short private directory: Unix socket paths are limited to ~108
		// bytes, which a run's state directory can come close to.
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		d, err := os.MkdirTemp(base, "arm-")
		if err != nil {
			return nil, err
		}
		m.sockDir = d
		m.inSandbox = map[string]string{}
	}
	for name, ch := range r.cfg.Models {
		var sock string
		if m.sockDir != "" {
			sock = filepath.Join(m.sockDir, name+".sock")
			m.inSandbox[name] = agentboxAddr[name]
		}
		// Only the proxy says which run a request is: the sandbox's own
		// RunHeader is always dropped, and only a ledger gets one.
		headers := http.Header{RunHeader: nil}
		var requestHeader string
		if ch.Ledger {
			// The broker sets the call's id, replacing any the sandbox sent.
			headers[RunHeader] = []string{runID}
			requestHeader = RequestHeader
		} else {
			headers[RequestHeader] = nil
		}
		metering, billing := ch.MeteringMode(), ch.billing()
		cfg := broker.Config{
			UnixSocket:      sock,
			Upstream:        ch.Upstream,
			ListenHost:      modelListenHost(),
			AuthHeader:      ch.AuthHeader,
			AuthValue:       ch.AuthValue,
			Credential:      ch.Credential,
			Headers:         headers,
			PathPrefixes:    ch.Paths,
			MaxRequestB:     64 << 20, // a long conversation's request is several MB
			RatePerMinute:   600,
			RequestIDHeader: requestHeader,
			// Each refusal, with why and the path asked for (forwarded
			// calls are OnForward's). No MaxTotalB is set, so every refusal
			// comes through here.
			OnRefuse: func(kind, reason, path string) {
				r.auditModel(runID, name, kind, reason, path)
				r.emit(Event{Kind: "sandbox.model_" + kind, RunID: runID, ThreadID: threadID, Detail: name + " " + reason + " " + path})
			},
			OnForward: func(id string) {
				r.audit(runID, audit.TypeModelForwarded, audit.ModelDetail{Provider: name, Reason: "forwarded", Metering: metering, RequestID: id})
			},
		}
		// The use of a broker-metered call is on record only because the
		// proxy reads it from the response.
		if metering == audit.MeteringBroker {
			cfg.UsageFormat = usageFormat(name)
			if spend != nil {
				cfg.Exhausted = spend.exhausted
			}
			cfg.OnUsage = func(id string, u broker.Usage) {
				spend.charge(name, u)
				r.audit(runID, audit.TypeModelUsage, audit.UsageDetail{RequestID: id, Provider: name, Model: u.Model,
					InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
					CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
					Billing: billing, Truncated: u.Truncated})
			}
		}
		p, err := broker.Start(cfg)
		if err != nil {
			m.close()
			return nil, err
		}
		m.proxies[name] = p
		if name == ProviderOpenAI && ch.ChatGPT {
			m.chatgpt = true
		}
		if name == ProviderAnthropic && ch.OAuth {
			m.anthropicOAuth = true
		}
	}
	return m, nil
}

// modelSpend is one run's use of its broker-metered channels against
// Config.ModelBudget, in USD millionths at list price: what the run's calls
// would cost billed per token, subscription channels included, so the
// budget caps a runaway run whatever pays for it.
type modelSpend struct {
	budget int64
	spent  atomic.Int64
}

// BudgetMicros converts a budget in USD to millionths (0 for none).
func BudgetMicros(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(usd * 1e6)
}

// charge adds one call's use. A model the price table does not name (or a
// response that never said which) is charged at its provider's highest
// rates, so it cannot slip past the budget.
func (s *modelSpend) charge(provider string, u broker.Usage) {
	if s == nil {
		return
	}
	t := pricing.Default()
	p, ok := t.Lookup(provider, u.Model)
	if !ok {
		p, _ = t.Ceiling(provider)
	}
	s.spent.Add(p.Micros(pricing.Tokens{Input: u.InputTokens, Output: u.OutputTokens,
		CacheRead: u.CacheReadTokens, CacheWrite: u.CacheWriteTokens}))
}

// exhausted: the run has used its budget; the proxies refuse what follows.
func (s *modelSpend) exhausted() bool { return s.spent.Load() >= s.budget }

// modelSockets: the proxies are reached over Unix sockets and a forwarder
// (Linux: gVisor has its own network stack), else at their loopback ports
// (macOS). A var so tests cover both paths on any OS.
var modelSockets = goruntime.GOOS == "linux"

// agentboxAddr is where the forwarder inside a gVisor sandbox serves each
// provider's proxy (the sandbox's own loopback): 4801, 4802, then one port
// per CompatProvider from 4803.
var agentboxAddr = func() map[string]string {
	m := map[string]string{ProviderAnthropic: "127.0.0.1:4801", ProviderOpenAI: "127.0.0.1:4802"}
	for i, p := range CompatProviders {
		m[p.Name] = "127.0.0.1:" + strconv.Itoa(4803+i)
	}
	return m
}()

// url is the base URL a CLI in the sandbox reaches provider name's proxy at.
func (m *modelRun) url(name string) string {
	if a, ok := m.inSandbox[name]; ok {
		return "http://" + a
	}
	return m.proxies[name].BaseURL()
}

// routes maps the forwarder's in-sandbox addresses to the proxies' sockets
// (Linux); nil otherwise.
func (m *modelRun) routes() map[string]string {
	if m == nil || m.sockDir == "" {
		return nil
	}
	r := map[string]string{}
	for name, a := range m.inSandbox {
		r[a] = filepath.Join(m.sockDir, name+".sock")
	}
	return r
}

// ports are the proxies' loopback ports (macOS); nil otherwise.
func (m *modelRun) ports() []int {
	if m == nil || m.sockDir != "" {
		return nil
	}
	var out []int
	for _, p := range m.proxies {
		u, err := url.Parse(p.BaseURL())
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(u.Port()); err == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// envAllow drops the channels' credential variables from an env allowlist.
func (m *modelRun) envAllow(allow []string) []string {
	return slices.DeleteFunc(slices.Clone(allow), func(k string) bool {
		for name := range m.proxies {
			if slices.Contains(modelCredentialEnv[name], k) {
				return true
			}
		}
		return false
	})
}

// cliEnv points each CLI at its proxies: claude at Anthropic's, codex at
// OpenAI's. pi is pointed at them through models.json (writePiModels) and
// takes the run tokens from env.
func (m *modelRun) cliEnv(channels map[string]*ModelChannel) map[string][]string {
	env := map[string][]string{}
	if p := m.proxies[ProviderAnthropic]; p != nil {
		env["claude"] = []string{"ANTHROPIC_BASE_URL=" + m.url(ProviderAnthropic), "ANTHROPIC_AUTH_TOKEN=" + p.Token()}
		if channels[ProviderAnthropic].OAuth {
			env["pi"] = append(env["pi"], "ANTHROPIC_OAUTH_TOKEN="+p.Token())
		} else {
			env["pi"] = append(env["pi"], piTokenEnv(ProviderAnthropic)+"="+p.Token())
		}
	}
	if p := m.proxies[ProviderOpenAI]; p != nil {
		// codex reads its run token from the env var its config names
		// (writeCodexConfig).
		env["codex"] = []string{codexKeyEnv + "=" + p.Token()}
		if !m.chatgpt { // pi speaks the public API, not ChatGPT's backend
			env["pi"] = append(env["pi"], piTokenEnv(ProviderOpenAI)+"="+p.Token())
		}
	}
	for _, cp := range CompatProviders {
		if p := m.proxies[cp.Name]; p != nil {
			env["pi"] = append(env["pi"], piTokenEnv(cp.Name)+"="+p.Token())
		}
	}
	return env
}

// codexKeyEnv carries codex's run token; codexProvider is the provider
// writeCodexConfig defines.
const (
	codexKeyEnv   = "AGENT_RUNNER_MODEL_TOKEN"
	codexProvider = "agent-runner-proxy"
)

// writeCodexConfig writes the run's config.toml in codex's sandbox CODEX_HOME:
// a model provider whose base_url is the OpenAI proxy, made the default.
// codex ignores OPENAI_BASE_URL for its built-in provider (0.149 connects to
// wss://api.openai.com directly), so a custom provider is the way to route it.
// Without an OpenAI channel, any old file is removed.
func (m *modelRun) writeCodexConfig(home, rel string) error {
	root, err := safefs.Open(home) // the agent's to write: no link followed
	if err != nil {
		return err
	}
	defer root.Close()
	path := filepath.Join(rel, "config.toml")
	p := m.proxies[ProviderOpenAI]
	if p == nil {
		if err := root.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	cfg := fmt.Sprintf(`# Written by agent-runner for this sandbox run: codex reaches OpenAI only
# through the run's model proxy, which holds the API key.
model_provider = %q

[model_providers.%s]
name = "OpenAI (agent-runner proxy)"
base_url = %q
env_key = %q
wire_api = "responses"
`, codexProvider, codexProvider, m.url(ProviderOpenAI)+"/v1", codexKeyEnv)
	if m.chatgpt {
		// The proxy holds a ChatGPT login and adds its token and account id.
		// name "OpenAI" keeps codex's OpenAI-only request features (codex
		// matches the provider by that name); no websockets through the proxy.
		cfg = fmt.Sprintf(`# Written by agent-runner for this sandbox run: codex reaches ChatGPT only
# through the run's model proxy, which holds the ChatGPT login.
model_provider = %q

[model_providers.%s]
name = "OpenAI"
base_url = %q
env_key = %q
wire_api = "responses"
supports_websockets = false
`, codexProvider, codexProvider, m.url(ProviderOpenAI)+chatgptBase, codexKeyEnv)
	}
	return safefs.WriteIn(root, path, []byte(cfg), 0o600)
}

// piTokenEnv carries pi's run token for a provider. Not the provider's
// standard key name (DEEPSEEK_API_KEY): that one is the agent's, for the
// programs it builds; pi reads this one through the run's auth.json
// (writePiAuth), which outranks the environment.
func piTokenEnv(provider string) string {
	return "AGENT_RUNNER_PI_" + strings.ToUpper(provider) + "_TOKEN"
}

// writePiAuth writes pi's auth.json for the run: each proxied provider's
// key is its run token, read from piTokenEnv. pi ranks auth.json above the
// environment, so the agent's own DEEPSEEK_API_KEY (an app's) never
// replaces it. The host's auth.json (its logins) is never seeded.
func (m *modelRun) writePiAuth(home, rel string) error {
	doc := map[string]any{}
	for name := range m.proxies {
		if (name == ProviderOpenAI && m.chatgpt) || (name == ProviderAnthropic && m.anthropicOAuth) {
			continue
		}
		doc[name] = map[string]string{"type": "api_key", "key": "$" + piTokenEnv(name)}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return safefs.Write(home, filepath.Join(rel, "auth.json"), b, 0o600) // the agent's home: no link followed
}

// writePiModels writes pi's models.json for the run: the seed's (host or
// agent-home) custom providers, with each proxied built-in provider's
// baseUrl pointed at its proxy and any apiKey setting removed (the run token
// comes from env).
func (m *modelRun) writePiModels(seedDir, home, rel string) error {
	doc := map[string]any{}
	if seedDir != "" {
		if b, err := os.ReadFile(filepath.Join(seedDir, "models.json")); err == nil {
			if err := json.Unmarshal(b, &doc); err != nil {
				return err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	for name := range m.proxies {
		if name == ProviderOpenAI && m.chatgpt {
			continue // ChatGPT's codex backend is not an API pi speaks
		}
		pc, _ := providers[name].(map[string]any)
		if pc == nil {
			pc = map[string]any{}
		}
		base := m.url(name)
		if name == ProviderOpenAI {
			base += "/v1"
		}
		pc["baseUrl"] = base
		delete(pc, "apiKey")
		providers[name] = pc
	}
	doc["providers"] = providers
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return safefs.Write(home, filepath.Join(rel, "models.json"), b, 0o600) // the agent's home: no link followed
}

// names lists the started channels, for the log.
func (m *modelRun) names() string {
	var n []string
	for name := range m.proxies {
		n = append(n, name)
	}
	slices.Sort(n)
	return strings.Join(n, ",")
}

// modelListenHost is where the proxies' TCP listeners are: loopback. On
// macOS the sandbox shares the host's network stack and reaches them there;
// on Linux it uses the Unix sockets. A proxy accepts only its run's token,
// which lives as long as the run.
func modelListenHost() string { return "127.0.0.1" }
