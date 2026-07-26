package chatcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirTemp runs the test from a temp runner directory.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	return dir
}

func TestInstallMCP_NoDeclaration_ActionableError(t *testing.T) {
	dir := chdirTemp(t)
	env := setupTestEnv(t)
	c := NewCommander(env.cfg, env.rt)

	reply, _, handled := c.Handle("/install-mcp maildirx", nil)
	if !handled {
		t.Fatal("expected /install-mcp to be handled")
	}
	// The error names the exact expected path and the fixing command.
	if !strings.Contains(reply, "mcp.json") || !strings.Contains(reply, "/bootstrap") {
		t.Errorf("error not actionable: %s", reply)
	}
	if !strings.Contains(reply, filepath.Base(dir)) && !strings.Contains(reply, "mcp.json") {
		t.Errorf("expected path context in: %s", reply)
	}
}

func TestBootstrap_CreatesAgentTemplates(t *testing.T) {
	chdirTemp(t)
	env := setupTestEnv(t)
	c := NewCommander(env.cfg, env.rt)

	reply, _, handled := c.Handle("/bootstrap", nil)
	if !handled {
		t.Fatal("expected /bootstrap to be handled")
	}
	if _, err := os.Stat("mcp.json.example"); err != nil {
		t.Errorf("mcp.json.example not created: %v (reply: %s)", err, reply)
	}
	if _, err := os.Stat(".env"); err != nil {
		t.Errorf(".env not created: %v (reply: %s)", err, reply)
	}
	data, _ := os.ReadFile(".env")
	if !strings.Contains(string(data), "AGENT_ISOLATED=true") {
		t.Errorf(".env missing isolation default: %s", data)
	}

	// Operator files are never overwritten.
	if err := os.WriteFile("mcp.json", []byte(`{"servers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, handled := c.Handle("/bootstrap", nil); !handled {
		t.Fatal("expected second /bootstrap to be handled")
	}
	if _, err := os.Stat("mcp.json.example"); err == nil {
		// Existing mcp.json means no new example is needed, but an earlier
		// one may remain — either state is fine; just ensure mcp.json intact.
		data, _ := os.ReadFile("mcp.json")
		if string(data) != `{"servers":{}}` {
			t.Errorf("mcp.json was modified: %s", data)
		}
	}
}

func TestConfig_ShowsIdentityAndMCP(t *testing.T) {
	dir := chdirTemp(t)
	env := setupTestEnv(t)
	env.cfg.Agent.Isolated = true
	c := NewCommander(env.cfg, env.rt)

	if err := os.WriteFile("mcp.json",
		[]byte(`{"servers":{"maildirx":{"command":"/bin/maildirx"}}}`), 0644); err != nil {
		t.Fatal(err)
	}

	reply, _, handled := c.Handle("/config", nil)
	if !handled {
		t.Fatal("expected /config to be handled")
	}
	for _, want := range []string{filepath.Base(dir), "isolated", "maildirx"} {
		if !strings.Contains(reply, want) {
			t.Errorf("config missing %q:\n%s", want, reply)
		}
	}
}
