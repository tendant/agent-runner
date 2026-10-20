package chatcmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/config"
)

// "/secrets" works as "/secret"; a near miss names the command meant and
// saves nothing, without repeating what was typed.
func TestSecretSlips(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secrets.json")
	c := &Commander{cfg: &config.Config{SecretsFile: file}}
	if reply, _, ok := c.Handle("/secrets set GITEE_TOKEN tok-1234567890 Gitee", nil); !ok || !strings.HasPrefix(reply, "ok: GITEE_TOKEN set") {
		t.Fatalf("/secrets set: %v %q", ok, reply)
	}
	for _, typo := range []string{"/secert set OTHER tok-abcdefghij", "/scret set OTHER tok-abcdefghij"} {
		reply, _, ok := c.Handle(typo, nil)
		if !ok || !strings.Contains(reply, "Did you mean /secret?") || strings.Contains(reply, "tok-") {
			t.Errorf("%s: %v %q", typo, ok, reply)
		}
	}
	list, _ := agentsecrets.New(file).List()
	if len(list) != 1 || list[0].Name != "GITEE_TOKEN" {
		t.Fatalf("secrets = %+v, want only GITEE_TOKEN", list)
	}
}

// A masked copy (a bot that missed the sealed original) is never saved.
func TestSecretMaskedValueRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secrets.json")
	c := &Commander{cfg: &config.Config{SecretsFile: file}}
	if reply, _, _ := c.Handle("/secret set GITEE_TOKEN ••••••", nil); !strings.Contains(reply, "masked copy") {
		t.Fatalf("reply %q", reply)
	}
	if reply, _, _ := c.Handle("/set ANTHROPIC_API_KEY ••••••", nil); !strings.Contains(reply, "masked copy") {
		t.Fatalf("/set reply %q", reply)
	}
	if list, _ := agentsecrets.New(file).List(); len(list) != 0 {
		t.Fatalf("saved %+v", list)
	}
}

// The model key names are free for the agent's own keys (an app's), except
// ANTHROPIC_API_KEY while the agent CLI is claude.
func TestSecretModelKeyNames(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secrets.json")
	cfg := &config.Config{SecretsFile: file}
	cfg.Agent.CLI = "pi"
	c := &Commander{cfg: cfg}
	if reply, _, _ := c.Handle("/secret set DEEPSEEK_API_KEY sk-app-1234567890 the app's DeepSeek key", nil); !strings.HasPrefix(reply, "ok: DEEPSEEK_API_KEY set") {
		t.Fatalf("pi: %q", reply)
	}
	cfg.Agent.CLI = "claude"
	if reply, _, _ := c.Handle("/secret set ANTHROPIC_API_KEY sk-ant-app-1234567890", nil); !strings.Contains(reply, "stays agent-runner's while the agent CLI is claude") {
		t.Fatalf("claude: %q", reply)
	}
	if reply, _, _ := c.Handle("/secret set DEEPSEEK_BASE_URL https://evil.example", nil); !strings.Contains(reply, "agent-runner's own") {
		t.Fatalf("a setting name was allowed: %q", reply)
	}
}
