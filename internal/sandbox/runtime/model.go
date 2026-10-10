package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/proxy"
)

// Providers with a model channel.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

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
}

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

// modelRun is a run's started channels: per provider, the proxy.
type modelRun struct {
	proxies map[string]*proxy.Proxy
	chatgpt bool // the OpenAI channel is a ChatGPT login (codex only)
}

func (m *modelRun) close() {
	for _, p := range m.proxies {
		_ = p.Close()
	}
}

// startModels starts a proxy for each configured provider, or none when the
// policy has no outbound network, which leaves a proxy unreachable (isobox
// cannot open a single port into the sandbox).
func (r *Runtime) startModels(spec sandbox.SandboxSpec, runID, threadID string) (*modelRun, error) {
	if len(r.cfg.Models) == 0 || spec.Network.Egress != sandbox.EgressOutbound {
		return nil, nil
	}
	m := &modelRun{proxies: map[string]*proxy.Proxy{}}
	for name, ch := range r.cfg.Models {
		p, err := proxy.Start(proxy.Config{
			Upstream:      ch.Upstream,
			ListenHost:    modelListenHost(),
			AuthHeader:    ch.AuthHeader,
			AuthValue:     ch.AuthValue,
			Credential:    ch.Credential,
			PathPrefixes:  ch.Paths,
			MaxRequestB:   64 << 20, // a long conversation's request is several MB
			RatePerMinute: 600,
			OnEvent: func(kind string) {
				if kind != "forwarded" { // per request: too chatty
					r.emit(Event{Kind: "sandbox.model_" + kind, RunID: runID, ThreadID: threadID, Detail: name})
				}
			},
		})
		if err != nil {
			m.close()
			return nil, err
		}
		m.proxies[name] = p
		if name == ProviderOpenAI && ch.ChatGPT {
			m.chatgpt = true
		}
	}
	return m, nil
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
		env["claude"] = []string{"ANTHROPIC_BASE_URL=" + p.BaseURL(), "ANTHROPIC_AUTH_TOKEN=" + p.Token()}
		if channels[ProviderAnthropic].OAuth {
			env["pi"] = append(env["pi"], "ANTHROPIC_OAUTH_TOKEN="+p.Token())
		} else {
			env["pi"] = append(env["pi"], "ANTHROPIC_API_KEY="+p.Token())
		}
	}
	if p := m.proxies[ProviderOpenAI]; p != nil {
		// codex reads its run token from the env var its config names
		// (writeCodexConfig).
		env["codex"] = []string{codexKeyEnv + "=" + p.Token()}
		if !m.chatgpt { // pi speaks the public API, not ChatGPT's backend
			env["pi"] = append(env["pi"], "OPENAI_API_KEY="+p.Token())
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
func (m *modelRun) writeCodexConfig(codexDir string) error {
	path := filepath.Join(codexDir, "config.toml")
	p := m.proxies[ProviderOpenAI]
	if p == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
`, codexProvider, codexProvider, p.BaseURL()+"/v1", codexKeyEnv)
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
`, codexProvider, codexProvider, p.BaseURL()+chatgptBase, codexKeyEnv)
	}
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(cfg), 0o600)
}

// writePiModels writes pi's models.json for the run: the seed's (host or
// agent-home) custom providers, with each proxied built-in provider's
// baseUrl pointed at its proxy and any apiKey setting removed (the run token
// comes from env).
func (m *modelRun) writePiModels(seedDir, piDir string) error {
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
	for name, p := range m.proxies {
		if name == ProviderOpenAI && m.chatgpt {
			continue // ChatGPT's codex backend is not an API pi speaks
		}
		pc, _ := providers[name].(map[string]any)
		if pc == nil {
			pc = map[string]any{}
		}
		base := p.BaseURL()
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
	if err := os.MkdirAll(piDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(piDir, "models.json"), b, 0o600)
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

// modelListenHost is where the proxies listen. Seatbelt shares the host's
// network stack, so loopback. gVisor has its own, where 127.0.0.1 is the
// sandbox itself; it reaches the host by the host's address. A proxy accepts
// only its run's token, which lives as long as the run.
func modelListenHost() string {
	if goruntime.GOOS == "darwin" {
		return "127.0.0.1"
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				return n.IP.String()
			}
		}
	}
	return "127.0.0.1"
}
