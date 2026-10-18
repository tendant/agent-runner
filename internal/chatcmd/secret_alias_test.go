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
