package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
)

func TestModelChannelKeepsTheCredentialOutOfTheSandbox(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "real-secret")
	t.Setenv("ANTHROPIC_API_KEY", "real-key")

	got := &sandbox.ProcSpec{}
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Second,
		EnvAllow:   []string{"ANTHROPIC_API_KEY"}, // listed, but the channel keeps it out
		Model:      &ModelChannel{Upstream: u, AuthHeader: "Authorization", AuthValue: "Bearer real-secret"},
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{nil, got} }})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := executor.LauncherFrom(run.Ctx)
	_, release, err := l.Command(run.Ctx, executor.LaunchSpec{Name: "claude", Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	release()
	env := strings.Join(got.Env, "\n")
	if strings.Contains(env, "real-secret") || strings.Contains(env, "real-key") {
		t.Fatalf("a credential entered the sandbox: %s", env)
	}
	var base, token string
	for _, e := range got.Env {
		if v, ok := strings.CutPrefix(e, "ANTHROPIC_BASE_URL="); ok {
			base = v
		}
		if v, ok := strings.CutPrefix(e, "ANTHROPIC_AUTH_TOKEN="); ok {
			token = v
		}
	}
	if base == "" || token == "" {
		t.Fatalf("claude has no proxy URL or token: %s", env)
	}

	// Through the proxy with the run's token: the upstream sees the credential.
	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || gotAuth != "Bearer real-secret" {
		t.Errorf("status %d, upstream auth %q", resp.StatusCode, gotAuth)
	}
	// Without the token, or off the allowed paths: refused.
	for _, r := range []struct{ path, token string }{{"/v1/messages", "wrong"}, {"/v1/files", token}} {
		req, _ := http.NewRequest("POST", base+r.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+r.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s with token %q: status %d, want refused", r.path, r.token, resp.StatusCode)
		}
	}

	run.Finish()
	if _, err := http.Get(base + "/api/hello"); err == nil {
		t.Error("the proxy must stop with the run")
	}
}

func TestNoModelChannelWithoutOutboundNetwork(t *testing.T) {
	u, _ := url.Parse("https://api.anthropic.com")
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Second,
		Model: &ModelChannel{Upstream: u, AuthHeader: "x-api-key", AuthValue: "k"}})
	if err != nil {
		t.Fatal(err)
	}
	none := sandbox.SandboxSpec{Network: sandbox.NetworkPolicy{Egress: sandbox.EgressNone}}
	if p, err := rt.startModelProxy(none, "r", "t"); p != nil || err != nil {
		t.Errorf("egress none: proxy %v, err %v; want none (it would be unreachable)", p, err)
	}
}

func TestWithoutModelCredentials(t *testing.T) {
	got := withoutModelCredentials([]string{"FOO", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "BAR"})
	if strings.Join(got, ",") != "FOO,BAR" {
		t.Errorf("got %v", got)
	}
}
