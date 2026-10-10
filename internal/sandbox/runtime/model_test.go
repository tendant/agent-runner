package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// upstream is a fake provider recording the credential it received.
func upstream(t *testing.T, header string) (*url.URL, *string) {
	t.Helper()
	got := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get(header)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u, got
}

func envOf(spec *sandbox.ProcSpec) map[string]string {
	m := map[string]string{}
	for _, e := range spec.Env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// launch returns the sandbox env one CLI is started with.
func launch(t *testing.T, run *Run, got *sandbox.ProcSpec, cli string) map[string]string {
	t.Helper()
	l, _ := executor.LauncherFrom(run.Ctx)
	_, release, err := l.Command(run.Ctx, executor.LaunchSpec{Name: cli})
	if err != nil {
		t.Fatal(err)
	}
	release()
	return envOf(got)
}

func post(t *testing.T, url, auth string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestModelChannelsKeepCredentialsOutOfTheSandbox(t *testing.T) {
	anthropic, anthropicGot := upstream(t, "Authorization")
	openai, openaiGot := upstream(t, "Authorization")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "real-claude-token")
	t.Setenv("OPENAI_API_KEY", "real-openai-key")

	got := &sandbox.ProcSpec{}
	piSeed := t.TempDir()
	os.WriteFile(filepath.Join(piSeed, "models.json"),
		[]byte(`{"providers":{"anthropic":{"apiKey":"$ANTHROPIC_API_KEY"},"ollama":{"baseUrl":"http://localhost:11434/v1"}}}`), 0o600)
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Second,
		EnvAllow: []string{"OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}, // listed, but the channels keep them out
		PiSeed:   piSeed,
		Models: map[string]*ModelChannel{
			ProviderAnthropic: {Upstream: anthropic, AuthHeader: "Authorization", AuthValue: "Bearer real-claude-token", Paths: AnthropicPaths, OAuth: true},
			ProviderOpenAI:    {Upstream: openai, AuthHeader: "Authorization", AuthValue: "Bearer real-openai-key", Paths: OpenAIPaths},
		},
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{nil, got} }})
	if err != nil {
		t.Fatal(err)
	}
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	for _, cli := range []string{"claude", "codex", "pi", "opencode"} {
		env := launch(t, run, got, cli)
		for k, v := range env {
			if strings.Contains(v, "real-") {
				t.Errorf("%s: credential in the sandbox: %s", cli, k)
			}
		}
	}

	claude := launch(t, run, got, "claude")
	if code := post(t, claude["ANTHROPIC_BASE_URL"]+"/v1/messages", claude["ANTHROPIC_AUTH_TOKEN"]); code != 200 || *anthropicGot != "Bearer real-claude-token" {
		t.Errorf("claude through its proxy: %d, upstream got %q", code, *anthropicGot)
	}
	codex := launch(t, run, got, "codex")
	codexToken := codex[codexKeyEnv]
	conf, err := os.ReadFile(filepath.Join(codex["CODEX_HOME"], "config.toml"))
	if err != nil || codexToken == "" {
		t.Fatalf("codex: config %v, env %v", err, codex)
	}
	var codexBase string
	for _, line := range strings.Split(string(conf), "\n") {
		if v, ok := strings.CutPrefix(line, "base_url = "); ok {
			codexBase = strings.Trim(v, `"`)
		}
	}
	if !strings.Contains(string(conf), `model_provider = "`+codexProvider+`"`) || !strings.HasSuffix(codexBase, "/v1") {
		t.Fatalf("codex config.toml:\n%s", conf)
	}
	if code := post(t, codexBase+"/responses", codexToken); code != 200 || *openaiGot != "Bearer real-openai-key" {
		t.Errorf("codex through its proxy: %d, upstream got %q", code, *openaiGot)
	}
	if code := post(t, codexBase+"/files", codexToken); code < 400 {
		t.Errorf("a path off the allowlist: %d, want refused", code)
	}
	if !strings.HasPrefix(codex["CODEX_HOME"], "/") || !strings.Contains(codex["CODEX_HOME"], filepath.Join("home", "t1")) {
		t.Errorf("CODEX_HOME = %q, want the sandbox home", codex["CODEX_HOME"])
	}

	// pi: an OAuth run token, and models.json pointing both providers at
	// their proxies, keeping the seed's other providers.
	pi := launch(t, run, got, "pi")
	if pi["ANTHROPIC_OAUTH_TOKEN"] == "" || pi["OPENAI_API_KEY"] == "" || pi["ANTHROPIC_API_KEY"] != "" {
		t.Errorf("pi env: %v", pi)
	}
	b, err := os.ReadFile(filepath.Join(pi["PI_CODING_AGENT_DIR"], "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	json.Unmarshal(b, &doc)
	if doc.Providers["anthropic"]["baseUrl"] != claude["ANTHROPIC_BASE_URL"] || doc.Providers["anthropic"]["apiKey"] != nil {
		t.Errorf("pi anthropic provider = %v", doc.Providers["anthropic"])
	}
	if doc.Providers["openai"]["baseUrl"] != codexBase || doc.Providers["ollama"] == nil {
		t.Errorf("pi providers = %v", doc.Providers)
	}

	run.Finish()
	if _, err := http.Get(claude["ANTHROPIC_BASE_URL"] + "/api/hello"); err == nil {
		t.Error("the proxies must stop with the run")
	}
}

func TestNoModelChannelsWithoutOutboundNetwork(t *testing.T) {
	u, _ := url.Parse("https://api.anthropic.com")
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Second,
		Models: map[string]*ModelChannel{ProviderAnthropic: {Upstream: u, AuthHeader: "x-api-key", AuthValue: "k", Paths: AnthropicPaths}}})
	if err != nil {
		t.Fatal(err)
	}
	none := sandbox.SandboxSpec{Network: sandbox.NetworkPolicy{Egress: sandbox.EgressNone}}
	if m, err := rt.startModels(none, "r", "t"); m != nil || err != nil {
		t.Errorf("egress none: %v, %v; want no channels (they would be unreachable)", m, err)
	}
}

func TestChatGPTChannelIsCodexOnly(t *testing.T) {
	var gotPath, gotAuth, gotAcct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAcct = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cred := func(context.Context) (http.Header, error) {
		return http.Header{"Authorization": {"Bearer real-chatgpt-access"}, "Chatgpt-Account-Id": {"acct-1"}}, nil
	}

	got := &sandbox.ProcSpec{}
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Second,
		Models:     map[string]*ModelChannel{ProviderOpenAI: {Upstream: u, Paths: ChatGPTPaths, Credential: cred, ChatGPT: true}},
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{nil, got} }})
	if err != nil {
		t.Fatal(err)
	}
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Finish()

	codex := launch(t, run, got, "codex")
	conf, err := os.ReadFile(filepath.Join(codex["CODEX_HOME"], "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var base string
	for _, line := range strings.Split(string(conf), "\n") {
		if v, ok := strings.CutPrefix(line, "base_url = "); ok {
			base = strings.Trim(v, `"`)
		}
	}
	for _, want := range []string{`name = "OpenAI"`, "supports_websockets = false", `env_key = "` + codexKeyEnv + `"`} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("codex config.toml lacks %s:\n%s", want, conf)
		}
	}
	if !strings.HasSuffix(base, "/backend-api/codex") {
		t.Fatalf("base_url %q", base)
	}
	for _, p := range []string{"/responses", "/responses/compact", "/models"} {
		if code := post(t, base+p, codex[codexKeyEnv]); code != 200 || gotPath != "/backend-api/codex"+p ||
			gotAuth != "Bearer real-chatgpt-access" || gotAcct != "acct-1" {
			t.Errorf("%s: %d, upstream got %s %q %q", p, code, gotPath, gotAuth, gotAcct)
		}
	}
	if code := post(t, strings.TrimSuffix(base, "/codex")+"/wham/usage", codex[codexKeyEnv]); code < 400 {
		t.Errorf("a ChatGPT path off the allowlist: %d, want refused", code)
	}

	// pi cannot use ChatGPT's backend: no run token, no openai provider.
	pi := launch(t, run, got, "pi")
	if pi["OPENAI_API_KEY"] != "" {
		t.Errorf("pi got an OpenAI run token for a ChatGPT channel")
	}
	if b, err := os.ReadFile(filepath.Join(pi["PI_CODING_AGENT_DIR"], "models.json")); err == nil && strings.Contains(string(b), base) {
		t.Errorf("pi models.json points at the ChatGPT proxy: %s", b)
	}
}
