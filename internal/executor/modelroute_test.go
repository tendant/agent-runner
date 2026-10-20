package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// Each agent CLI gets agent-runner's model key by its own route, and the
// standard names leave the environment, free for the agent's own secrets.
func TestHostModelRoute(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir()) // no config.toml: codex on OpenAI
	env := []string{"PATH=/bin", "DEEPSEEK_API_KEY=runner-ds", "OPENAI_API_KEY=runner-oa", "ANTHROPIC_API_KEY=runner-an"}

	args, out := hostModelRoute("/usr/bin/pi", []string{"--mode", "rpc", "--provider", "deepseek"}, env)
	m := envMap(out)
	if flagValue(args, "--api-key") != "runner-ds" || m["DEEPSEEK_API_KEY"] != "" || m["OPENAI_API_KEY"] != "" || m["PATH"] != "/bin" {
		t.Errorf("pi: args %v env %v", args, m)
	}

	args, out = hostModelRoute("codex", []string{"exec", "--json", "-"}, env)
	m = envMap(out)
	if args[0] != "exec" || !slices.Contains(args, `model_provider="agent-runner-openai"`) || m["OPENAI_API_KEY"] != "" || m["AGENT_RUNNER_OPENAI_API_KEY"] != "runner-oa" {
		t.Errorf("codex: args %v env %v", args, m)
	}

	_, out = hostModelRoute("opencode", []string{"run", "x"}, env)
	m = envMap(out)
	if m["DEEPSEEK_API_KEY"] != "" || m["AGENT_RUNNER_DEEPSEEK_API_KEY"] != "runner-ds" || !strings.Contains(m["OPENCODE_CONFIG_CONTENT"], "{env:AGENT_RUNNER_DEEPSEEK_API_KEY}") {
		t.Errorf("opencode: env %v", m)
	}

	// claude has no route that outranks its environment: its key stays.
	_, out = hostModelRoute("claude", []string{"-p"}, env)
	m = envMap(out)
	if m["ANTHROPIC_API_KEY"] != "runner-an" || m["DEEPSEEK_API_KEY"] != "" {
		t.Errorf("claude: env %v", m)
	}

	// No route to give a key by: it stays where the CLI looks for it.
	_, out = hostModelRoute("pi", []string{"--mode", "rpc"}, env)
	if envMap(out)["DEEPSEEK_API_KEY"] != "runner-ds" {
		t.Error("pi without --provider lost its key")
	}
	os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(`model_provider = "azure"`+"\n"), 0o600)
	args, out = hostModelRoute("codex", []string{"exec", "-"}, env)
	if envMap(out)["OPENAI_API_KEY"] != "runner-oa" || slices.Contains(args, `model_provider="agent-runner-openai"`) {
		t.Errorf("codex on another provider: args %v env %v", args, envMap(out))
	}

	// Other programs are left alone.
	if _, out := hostModelRoute("git", nil, env); envMap(out)["DEEPSEEK_API_KEY"] != "runner-ds" {
		t.Error("a non-agent program lost its environment")
	}
}

// The real CLIs, through HostLauncher, against a fake model server: with the
// agent's own key of the same name in the environment (an app's), each CLI
// still calls the model with agent-runner's key. Skipped where a CLI is not
// installed.
func TestHostModelRoute_RealCLIs(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real CLIs")
	}
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"fake","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), auths...)
		auths = nil
		return out
	}

	cases := []struct {
		cli   string
		args  []string
		env   []string // the runner's environment
		agent []string // the agent's own secrets (ExtraEnv)
	}{
		{"pi", []string{"-p", "hi", "--no-session", "--provider", "deepseek", "--model", "deepseek-chat"},
			[]string{"DEEPSEEK_API_KEY=runner-key"}, []string{"DEEPSEEK_API_KEY=agent-secret"}},
		{"codex", []string{"exec", "--skip-git-repo-check", "hi"},
			[]string{"OPENAI_API_KEY=runner-key", "OPENAI_BASE_URL=" + srv.URL + "/v1"}, []string{"OPENAI_API_KEY=agent-secret"}},
		{"opencode", []string{"run", "-m", "deepseek/deepseek-chat", "hi"},
			[]string{"DEEPSEEK_API_KEY=runner-key"}, []string{"DEEPSEEK_API_KEY=agent-secret"}},
	}
	for _, c := range cases {
		t.Run(c.cli, func(t *testing.T) {
			if _, err := exec.LookPath(c.cli); err != nil {
				t.Skipf("%s not installed", c.cli)
			}
			home := t.TempDir()
			t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
			os.MkdirAll(os.Getenv("CODEX_HOME"), 0o700)
			piDir := filepath.Join(home, "pi")
			os.MkdirAll(piDir, 0o700)
			os.WriteFile(filepath.Join(piDir, "models.json"), []byte(`{"providers":{"deepseek":{"baseUrl":"`+srv.URL+`"}}}`), 0o600)
			// The model is declared, so opencode needn't fetch its catalog.
			ocContent, _ := json.Marshal(map[string]any{"provider": map[string]any{"deepseek": map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"options": map[string]any{"baseURL": srv.URL},
				"models":  map[string]any{"deepseek-chat": map[string]any{}},
			}}})
			for _, kv := range append(c.env,
				"PI_CODING_AGENT_DIR="+piDir, "PI_OFFLINE=1",
				"XDG_CONFIG_HOME="+filepath.Join(home, "cfg"), "XDG_DATA_HOME="+filepath.Join(home, "data"),
				"XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CACHE_HOME="+filepath.Join(home, "cache")) {
				k, v, _ := strings.Cut(kv, "=")
				t.Setenv(k, v)
			}
			agent := c.agent
			if c.cli == "opencode" {
				// The project's own config (the base URL), merged with
				// agent-runner's key route.
				os.WriteFile(filepath.Join(home, "opencode.json"), ocContent, 0o600)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd, release, err := HostLauncher{}.Command(ctx, LaunchSpec{Name: c.cli, Args: c.args, Dir: home, ExtraEnv: agent})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			cmd.Stdin = nil
			out, _ := cmd.CombinedOutput()
			got := seen()
			if len(got) == 0 {
				t.Fatalf("%s never reached the fake model server:\n%s", c.cli, tail(string(out), 1500))
			}
			for _, a := range got {
				if a != "Bearer runner-key" {
					t.Fatalf("%s called the model with %q, want agent-runner's key", c.cli, a)
				}
			}
		})
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
