package chatcmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretCommand(t *testing.T) {
	env := setupTestEnv(t)
	env.cfg.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	c := NewCommander(env.cfg, env.rt)

	if reply, _, _ := c.Handle("/secret", nil); !strings.Contains(reply, "no agent secrets") {
		t.Errorf("empty: %q", reply)
	}
	reply, _, _ := c.Handle("/secret set GITEE_TOKEN tok-abc123 Gitee API for oam-server", nil)
	if !strings.HasPrefix(reply, "ok") || strings.Contains(reply, "tok-abc123") {
		t.Errorf("set must not echo the value: %q", reply)
	}
	reply, _, _ = c.Handle("/secret list", nil)
	if !strings.Contains(reply, "GITEE_TOKEN: Gitee API for oam-server") || strings.Contains(reply, "tok-abc123") {
		t.Errorf("list: %q", reply)
	}
	if reply, _, _ := c.Handle("/secret set GIT_TOKEN x", nil); !strings.Contains(reply, "agent-runner's own") {
		t.Errorf("runner name: %q", reply)
	}
	if reply, _, _ := c.Handle("/secret remove GITEE_TOKEN", nil); !strings.HasPrefix(reply, "ok") {
		t.Errorf("remove: %q", reply)
	}
	if reply, _, _ := c.Handle("/secret set ONLYNAME", nil); !strings.Contains(reply, "usage") {
		t.Errorf("usage: %q", reply)
	}
}

func TestSetMasksAPIKey(t *testing.T) {
	if !isSensitiveKey("API_KEY") {
		t.Error("API_KEY is the runner's REST key: masked like the others")
	}
}
