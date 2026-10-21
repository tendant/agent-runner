package chatcmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/config"
)

func newEnvCommander(t *testing.T) (*Commander, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{SecretsFile: filepath.Join(dir, "secrets.json"), EnvFile: filepath.Join(dir, "env.json")}
	cfg.Agent.CLI = "pi"
	return &Commander{cfg: cfg}, cfg
}

func TestEnvPlainAndSecret(t *testing.T) {
	c, cfg := newEnvCommander(t)
	if reply, _, _ := c.Handle("/env", nil); !strings.Contains(reply, "environment is empty") {
		t.Errorf("empty: %q", reply)
	}
	if reply, _, _ := c.Handle("/env set APP_ENV staging where the app deploys", nil); reply != "agent's environment: APP_ENV=staging, from the next session." {
		t.Errorf("set: %q", reply)
	}
	if reply, _, _ := c.Handle(`/env set GREETING "hello world" said first`, nil); !strings.Contains(reply, "GREETING=hello world") {
		t.Errorf("quoted: %q", reply)
	}
	reply, _, _ := c.Handle("/env secret GITEE_TOKEN tok-abc123 Gitee API for oam-server", nil)
	if !strings.HasPrefix(reply, "agent's secret: GITEE_TOKEN") || strings.Contains(reply, "tok-abc123") {
		t.Errorf("secret must not echo the value: %q", reply)
	}
	reply, _, _ = c.Handle("/env", nil)
	for _, want := range []string{"APP_ENV=staging: where the app deploys", "GREETING=hello world: said first", "GITEE_TOKEN=•••••• (secret): Gitee API for oam-server"} {
		if !strings.Contains(reply, want) {
			t.Errorf("list lacks %q:\n%s", want, reply)
		}
	}
	if strings.Contains(reply, "tok-abc123") {
		t.Errorf("list shows a secret's value:\n%s", reply)
	}
	plain, secrets, _ := agentsecrets.Load(cfg.SecretsFile, cfg.EnvFile)
	if len(plain) != 2 || len(secrets) != 1 {
		t.Fatalf("stored plain %+v secrets %+v", plain, secrets)
	}

	// Setting a name as the other kind moves it.
	if reply, _, _ := c.Handle("/env secret APP_ENV prod-ish", nil); !strings.Contains(reply, "it was a plain setting; now secret") {
		t.Errorf("move to secret: %q", reply)
	}
	plain, secrets, _ = agentsecrets.Load(cfg.SecretsFile, cfg.EnvFile)
	if len(plain) != 1 || len(secrets) != 2 {
		t.Fatalf("after moving: plain %+v secrets %+v", plain, secrets)
	}
	if reply, _, _ := c.Handle("/env unset APP_ENV", nil); !strings.HasPrefix(reply, "ok: APP_ENV removed") {
		t.Errorf("unset: %q", reply)
	}
	if reply, _, _ := c.Handle("/env unset NOPE", nil); !strings.Contains(reply, "nothing named NOPE") {
		t.Errorf("unset missing: %q", reply)
	}
}

// A credential typed as a plain setting is refused, never echoed: a plain
// setting is shown everywhere.
func TestEnvSetRefusesACredential(t *testing.T) {
	c, cfg := newEnvCommander(t)
	reply, _, _ := c.Handle("/env set GH_TOKEN ghp_abcdefghijklmnopqrstuvwxyz0123", nil)
	if !strings.Contains(reply, "looks like a credential") || !strings.Contains(reply, "/env secret GH_TOKEN") || strings.Contains(reply, "ghp_") {
		t.Errorf("reply %q", reply)
	}
	if plain, _, _ := agentsecrets.Load(cfg.SecretsFile, cfg.EnvFile); len(plain) != 0 {
		t.Errorf("saved %+v", plain)
	}
	if reply, _, _ := c.Handle("/env set KEY line1\nline2", nil); !strings.Contains(reply, "a plain setting is one line") {
		t.Errorf("multi-line plain: %q", reply)
	}
}

// Names agent-runner reads are refused with where they go; the model key
// names are free for the agent's own keys, except ANTHROPIC_API_KEY while
// the agent CLI is claude.
func TestEnvNames(t *testing.T) {
	c, cfg := newEnvCommander(t)
	if reply, _, _ := c.Handle("/env set AGENT_CLI claude", nil); !strings.Contains(reply, "/config set") {
		t.Errorf("runner prefix: %q", reply)
	}
	if reply, _, _ := c.Handle("/env set API_KEY x", nil); !strings.Contains(reply, "/config set API_KEY") {
		t.Errorf("runner name: %q", reply)
	}
	if reply, _, _ := c.Handle("/env secret DEEPSEEK_API_KEY sk-app-1234567890 the app's DeepSeek key", nil); !strings.HasPrefix(reply, "agent's secret: DEEPSEEK_API_KEY") {
		t.Errorf("pi: %q", reply)
	}
	cfg.Agent.CLI = "claude"
	if reply, _, _ := c.Handle("/env secret ANTHROPIC_API_KEY sk-ant-app-1234567890", nil); !strings.Contains(reply, "stays agent-runner's while the agent CLI is claude") {
		t.Errorf("claude: %q", reply)
	}
}

// A value of several lines (a private key) comes on the lines after; a
// one-line value followed by a note takes the note as its description.
func TestEnvSecretMultiline(t *testing.T) {
	c, cfg := newEnvCommander(t)
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----"
	reply, _, _ := c.Handle("/env secret GITEE_SSH_KEY deploy key for oam\n"+key+"\n", nil)
	if !strings.HasPrefix(reply, "agent's secret") || strings.Contains(reply, "abc") {
		t.Fatalf("set: %q", reply)
	}
	c.Handle("/env secret GH_TOKEN ghp_secret123\nGitHub, read-only", nil)
	list, _ := agentsecrets.New(cfg.SecretsFile).List()
	if len(list) != 2 || list[0].Value != "ghp_secret123" || list[0].Description != "GitHub, read-only" ||
		list[1].Value != key || list[1].Description != "deploy key for oam" {
		t.Errorf("stored %+v", list)
	}
}

// A masked copy (a bot that missed the sealed original) is never saved.
func TestEnvAndConfigRefuseTheMask(t *testing.T) {
	c, cfg := newEnvCommander(t)
	if reply, _, _ := c.Handle("/env secret GITEE_TOKEN ••••••", nil); !strings.Contains(reply, "masked copy") {
		t.Fatalf("reply %q", reply)
	}
	if reply, _, _ := c.Handle("/config set ANTHROPIC_API_KEY ••••••", nil); !strings.Contains(reply, "masked copy") {
		t.Fatalf("/config set reply %q", reply)
	}
	if list, _ := agentsecrets.New(cfg.SecretsFile).List(); len(list) != 0 {
		t.Fatalf("saved %+v", list)
	}
}

// The replaced commands, and slips of them, say what to use and save
// nothing, without repeating what was typed.
func TestMovedCommands(t *testing.T) {
	c, cfg := newEnvCommander(t)
	for typed, want := range map[string]string{
		"/set AGENT_CLI claude":                   "/set is now /config set",
		"/secret set GITEE_TOKEN tok-1234567890":  "/secret is now /env secret",
		"/secrets set GITEE_TOKEN tok-1234567890": "/secret is now /env secret",
		"/secert set OTHER tok-abcdefghij":        "/secret is now /env secret",
		"/cofnig set AGENT_CLI claude":            "Did you mean /config?",
	} {
		reply, _, ok := c.Handle(typed, nil)
		if !ok || !strings.Contains(reply, want) || strings.Contains(reply, "tok-") || strings.Contains(reply, "claude") {
			t.Errorf("%s: %v %q", typed, ok, reply)
		}
	}
	if list, _ := agentsecrets.New(cfg.SecretsFile).List(); len(list) != 0 {
		t.Fatalf("saved %+v", list)
	}
}

func TestEnvHelp(t *testing.T) {
	c, _ := newEnvCommander(t)
	reply, _, _ := c.Handle("/env help", nil)
	for _, want := range []string{"/env set APP_ENV staging", "/env secret GITEE_TOKEN", "BEGIN OPENSSH PRIVATE KEY", "/repo add", "/config"} {
		if !strings.Contains(reply, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestSetMasksAPIKey(t *testing.T) {
	if !isSensitiveKey("API_KEY") {
		t.Error("API_KEY is the runner's REST key: masked like the others")
	}
}
