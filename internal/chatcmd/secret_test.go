package chatcmd

import (
	"github.com/agent-runner/agent-runner/internal/agentsecrets"
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
	if reply, _, _ := c.Handle("/secret set ONLYNAME", nil); !strings.Contains(reply, "needs a name and a value") || !strings.Contains(reply, "/secret set GITEE_TOKEN") {
		t.Errorf("usage: %q", reply)
	}
}

func TestSetMasksAPIKey(t *testing.T) {
	if !isSensitiveKey("API_KEY") {
		t.Error("API_KEY is the runner's REST key: masked like the others")
	}
}

// A value of several lines (a private key) comes on the lines after.
func TestSecretSetMultiline(t *testing.T) {
	env := setupTestEnv(t)
	env.cfg.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	c := NewCommander(env.cfg, env.rt)
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----"
	reply, _, _ := c.Handle("/secret set GITEE_SSH_KEY deploy key for oam\n"+key+"\n", nil)
	if !strings.HasPrefix(reply, "ok") || strings.Contains(reply, "abc") {
		t.Fatalf("set: %q", reply)
	}
	list, _ := agentsecrets.New(env.cfg.SecretsFile).List()
	if len(list) != 1 || list[0].Value != key || list[0].Description != "deploy key for oam" {
		t.Errorf("stored %+v", list)
	}
}

func TestSecretHelp(t *testing.T) {
	env := setupTestEnv(t)
	env.cfg.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	c := NewCommander(env.cfg, env.rt)
	reply, _, _ := c.Handle("/secret help", nil)
	for _, want := range []string{"/secret set GITEE_TOKEN", "BEGIN OPENSSH PRIVATE KEY", "/repo add", "only with\n/secret"} {
		if !strings.Contains(reply, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// A one-line value followed by a note: the note is the description.
func TestSecretSetValueThenNote(t *testing.T) {
	env := setupTestEnv(t)
	env.cfg.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	c := NewCommander(env.cfg, env.rt)
	c.Handle("/secret set GH_TOKEN ghp_secret123\nGitHub, read-only", nil)
	list, _ := agentsecrets.New(env.cfg.SecretsFile).List()
	if len(list) != 1 || list[0].Value != "ghp_secret123" || list[0].Description != "GitHub, read-only" {
		t.Errorf("stored %+v", list)
	}
}
