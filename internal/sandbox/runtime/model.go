package runtime

import (
	"net"
	"net/url"
	goruntime "runtime"
	"slices"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/proxy"
)

// ModelChannel is the provider credential a sandboxed claude CLI reaches the
// model with. The runtime starts a proxy (internal/sandbox/proxy) per run that
// attaches it; the CLI gets only the proxy's URL and a per-run token, so the
// credential never enters the sandbox.
type ModelChannel struct {
	Upstream   *url.URL // e.g. https://api.anthropic.com
	AuthHeader string   // "Authorization" (setup-token: "Bearer ...") or "x-api-key"
	AuthValue  string
}

// modelPaths are the requests the claude CLI makes to its API: the
// connectivity check and messages (with count_tokens under it).
var modelPaths = []string{"/v1/messages", "/api/hello"}

// modelCredentialEnv are the host variables that would hand the sandbox a
// provider credential or route around the proxy; with the channel on they are
// never passed in, whatever AGENT_SANDBOX_ENV_ALLOW lists.
var modelCredentialEnv = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"}

// startModelProxy starts the run's model channel, or returns nil when there
// is none to start: no credential configured, or a policy without outbound
// network, which leaves the proxy unreachable (isobox cannot open a single
// port into the sandbox).
func (r *Runtime) startModelProxy(spec sandbox.SandboxSpec, runID, threadID string) (*proxy.Proxy, error) {
	m := r.cfg.Model
	if m == nil || spec.Network.Egress != sandbox.EgressOutbound {
		return nil, nil
	}
	return proxy.Start(proxy.Config{
		Upstream:      m.Upstream,
		ListenHost:    modelListenHost(),
		AuthHeader:    m.AuthHeader,
		AuthValue:     m.AuthValue,
		PathPrefixes:  modelPaths,
		MaxRequestB:   64 << 20, // a long conversation's request is several MB
		RatePerMinute: 600,
		OnEvent: func(kind string) {
			if kind != "forwarded" { // per request: too chatty
				r.emit(Event{Kind: "sandbox.model_" + kind, RunID: runID, ThreadID: threadID})
			}
		},
	})
}

// modelListenHost is where the proxy listens. Seatbelt shares the host's
// network stack, so loopback. gVisor has its own, where 127.0.0.1 is the
// sandbox itself; it reaches the host by the host's address. The proxy
// accepts only the run's token, which lives as long as the run.
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

// withoutModelCredentials drops modelCredentialEnv from an env allowlist.
func withoutModelCredentials(allow []string) []string {
	return slices.DeleteFunc(slices.Clone(allow), func(k string) bool {
		return slices.Contains(modelCredentialEnv, k)
	})
}
