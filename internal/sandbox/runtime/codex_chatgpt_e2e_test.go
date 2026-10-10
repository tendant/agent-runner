package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// TestRealCodexThroughChatGPTChannel runs the real codex CLI (on the host)
// with the config and env a sandboxed run gets, against a fake ChatGPT codex
// backend behind the run's proxy. Opt-in: AGENT_E2E_REAL_CODEX=1.
func TestRealCodexThroughChatGPTChannel(t *testing.T) {
	if os.Getenv("AGENT_E2E_REAL_CODEX") == "" {
		t.Skip("set AGENT_E2E_REAL_CODEX=1 (needs the codex CLI; makes no real model call)")
	}
	type req struct{ path, auth, acct, upgrade, body string }
	var mu sync.Mutex
	var reqs []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		mu.Lock()
		reqs = append(reqs, req{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"), r.Header.Get("Upgrade"), string(b[:n])})
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"PONG-FROM-FAKE"}]}}`,
			`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1,"input_tokens_details":null,"output_tokens":1,"output_tokens_details":null,"total_tokens":2}}}`,
		} {
			typ := strings.SplitN(strings.SplitN(ev, `"type":"`, 2)[1], `"`, 2)[0]
			w.Write([]byte("event: " + typ + "\ndata: " + ev + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cred := func(context.Context) (http.Header, error) {
		return http.Header{"Authorization": {"Bearer fake-access"}, "Chatgpt-Account-Id": {"acct-1"}}, nil
	}
	got := &sandbox.ProcSpec{}
	rt, err := New(Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "test", LeaseTTL: time.Minute,
		Models:     map[string]*ModelChannel{ProviderOpenAI: {Upstream: u, Paths: ChatGPTPaths, Credential: cred, ChatGPT: true}},
		NewBackend: func(map[string]string) sandbox.Backend { return fakeBackend{nil, got} }})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", ThreadID: "t1", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Finish()
	env := launch(t, run, got, "codex")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "exec", "--skip-git-repo-check", "say pong")
	cmd.Dir = ws
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"CODEX_HOME=" + env["CODEX_HOME"], codexKeyEnv + "=" + env[codexKeyEnv]}
	out, err := cmd.CombinedOutput()
	t.Logf("codex exit: %v\n%s", err, out)

	mu.Lock()
	defer mu.Unlock()
	var sawResponses bool
	for _, r := range reqs {
		t.Logf("upstream got %s auth=%q acct=%q upgrade=%q", r.path, r.auth, r.acct, r.upgrade)
		if r.path == "/backend-api/codex/responses" {
			sawResponses = true
			if r.auth != "Bearer fake-access" || r.acct != "acct-1" || r.upgrade != "" {
				t.Errorf("responses request: %+v", r)
			}
			if !strings.Contains(r.body, `"model"`) && !strings.Contains(r.body, "\x28\xb5\x2f\xfd") {
				t.Logf("body starts: %q", r.body[:min(200, len(r.body))])
			}
		}
	}
	if !sawResponses {
		t.Fatal("codex never reached /backend-api/codex/responses through the proxy")
	}
	if !strings.Contains(string(out), "PONG-FROM-FAKE") {
		t.Errorf("codex did not print the fake model's reply")
	}
}
