package chatcmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
)

// agent-runner's own secrets: kept apart from the agent's, never shown, and
// removed with /config unset.
func TestConfigSecret(t *testing.T) {
	c, cfg := newEnvCommander(t)
	cfg.RunnerSecretsFile = filepath.Join(t.TempDir(), "runner-secrets.json")
	if reply, _, _ := c.Handle("/config secrets", nil); !strings.Contains(reply, "no secrets of its own") {
		t.Errorf("empty: %q", reply)
	}
	reply, _, _ := c.Handle("/config secret GITHUB_API_TOKEN ghp_abc123def GitHub API, added for the agent", nil)
	if !strings.Contains(reply, "GITHUB_API_TOKEN (13 characters, hidden), never the agent's") || strings.Contains(reply, "ghp_abc123def") {
		t.Errorf("set: %q", reply)
	}
	list, _ := agentsecrets.New(cfg.RunnerSecretsFile).List()
	if len(list) != 1 || list[0].Value != "ghp_abc123def" || list[0].Description != "GitHub API, added for the agent" {
		t.Errorf("stored %+v", list)
	}
	if _, agents, _ := agentsecrets.Load(cfg.SecretsFile, ""); len(agents) != 0 {
		t.Errorf("it reached the agent's secrets: %v", agents)
	}
	reply, _, _ = c.Handle("/config secrets", nil)
	if !strings.Contains(reply, "GITHUB_API_TOKEN=") || strings.Contains(reply, "ghp_abc123def") {
		t.Errorf("list: %q", reply)
	}

	// The same name as an agent's /env secret: allowed, each its own, said so.
	c.Handle("/env secret NPM_TOKEN npm_agent npm", nil)
	if reply, _, _ := c.Handle("/config secret NPM_TOKEN npm_runner", nil); !strings.Contains(reply, "also has an /env secret of that name") {
		t.Errorf("same name: %q", reply)
	}

	if reply, _, _ := c.Handle("/config unset GITHUB_API_TOKEN", nil); !strings.Contains(reply, "its secret GITHUB_API_TOKEN removed") {
		t.Errorf("unset: %q", reply)
	}
	for _, bad := range []string{"/config secret AGENT_CLI x", "/config secret ONLYNAME"} {
		if reply, _, _ := c.Handle(bad, nil); !strings.HasPrefix(reply, "error") {
			t.Errorf("%s: %q", bad, reply)
		}
	}
}
