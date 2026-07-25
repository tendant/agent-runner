package mcpsetup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()

	// Missing file is a normal state.
	cfg, err := Load(filepath.Join(dir, "absent.json"))
	if err != nil || cfg != nil {
		t.Fatalf("missing file: got %v, %v", cfg, err)
	}

	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, `{"servers":{"maildirx":{"command":"~/go/bin/maildirx","args":["mcp"],"env":{"MAIL_ROOT":"~/Mail"}}}}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := cfg.Servers["maildirx"]
	home, _ := os.UserHomeDir()
	if srv.Command != filepath.Join(home, "go/bin/maildirx") {
		t.Errorf("~ not expanded in command: %q", srv.Command)
	}
	if srv.Env["MAIL_ROOT"] != filepath.Join(home, "Mail") {
		t.Errorf("~ not expanded in env: %q", srv.Env["MAIL_ROOT"])
	}

	// A server without a command is a config error.
	writeFile(t, path, `{"servers":{"bad":{}}}`)
	if _, err := Load(path); err == nil {
		t.Error("expected error for server without command")
	}
}

func TestEnsureOpencode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	writeFile(t, path, `{"provider":{"openai":{"apiKey":"k"}}}`)

	srv := Server{Command: "/bin/maildirx", Args: []string{"mcp"}, Env: map[string]string{"MAIL_ROOT": "/mail"}}

	action, err := ensureOpencodeAt(path, "maildirx", srv)
	if err != nil || action != "registered" {
		t.Fatalf("first ensure: %s, %v", action, err)
	}

	// Existing unrelated config survives, entry is correct.
	var cfg map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["provider"]; !ok {
		t.Error("unrelated config was dropped")
	}
	entry := cfg["mcp"].(map[string]any)["maildirx"].(map[string]any)
	if entry["type"] != "local" || entry["enabled"] != true {
		t.Errorf("bad entry: %v", entry)
	}

	// Idempotent.
	if action, err = ensureOpencodeAt(path, "maildirx", srv); err != nil || action != "present" {
		t.Fatalf("second ensure: %s, %v", action, err)
	}

	// Drift converges.
	srv.Env["MAILDIRX_MCP_MODE"] = "read-only"
	if action, err = ensureOpencodeAt(path, "maildirx", srv); err != nil || action != "updated" {
		t.Fatalf("drift ensure: %s, %v", action, err)
	}

	// Missing file is created.
	fresh := filepath.Join(t.TempDir(), "sub", "opencode.json")
	if action, err = ensureOpencodeAt(fresh, "maildirx", srv); err != nil || action != "registered" {
		t.Fatalf("fresh ensure: %s, %v", action, err)
	}
}

func TestEnsureCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "model = \"gpt-5\"\n")

	srv := Server{Command: "/bin/maildirx", Args: []string{"mcp"}, Env: map[string]string{"MAIL_ROOT": "/mail", "A": "b"}}

	action, err := ensureCodexAt(path, "maildirx", srv)
	if err != nil || action != "registered" {
		t.Fatalf("first ensure: %s, %v", action, err)
	}
	data, _ := os.ReadFile(path)
	content := string(data)
	for _, want := range []string{
		"model = \"gpt-5\"",
		"[mcp_servers.maildirx]",
		`command = "/bin/maildirx"`,
		`args = ["mcp"]`,
		`A = "b", MAIL_ROOT = "/mail"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("config missing %q:\n%s", want, content)
		}
	}

	// Idempotent: no duplicate section.
	if action, err = ensureCodexAt(path, "maildirx", srv); err != nil || action != "present" {
		t.Fatalf("second ensure: %s, %v", action, err)
	}
	if strings.Count(mustRead(t, path), "[mcp_servers.maildirx]") != 1 {
		t.Error("duplicate section appended")
	}
}

func TestEnsureClaude(t *testing.T) {
	var calls [][]string
	registered := false
	orig := runCommand
	runCommand = func(extraEnv []string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append(append([]string{}, extraEnv...), append([]string{name}, args...)...))
		if len(args) >= 2 && args[0] == "mcp" && args[1] == "get" {
			if registered {
				return nil, nil
			}
			return nil, fmt.Errorf("not found")
		}
		registered = true
		return nil, nil
	}
	defer func() { runCommand = orig }()

	srv := Server{Command: "/bin/maildirx", Args: []string{"mcp"}, Env: map[string]string{"MAIL_ROOT": "/mail"}}

	action, err := ensureClaude("maildirx", srv)
	if err != nil || action != "registered" {
		t.Fatalf("first ensure: %s, %v", action, err)
	}
	want := []string{"claude", "mcp", "add", "--scope", "user", "maildirx", "--env", "MAIL_ROOT=/mail", "--", "/bin/maildirx", "mcp"}
	if got := strings.Join(calls[1], " "); got != strings.Join(want, " ") {
		t.Errorf("add args:\n got %s\nwant %s", got, strings.Join(want, " "))
	}

	if action, err = ensureClaude("maildirx", srv); err != nil || action != "present" {
		t.Fatalf("second ensure: %s, %v", action, err)
	}
}

func TestEnsureUnknownCLI(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{"x": {Command: "/bin/x"}}}
	results := Ensure("vim", cfg)
	if len(results) != 1 || results[0].Action != "error" {
		t.Fatalf("expected error result, got %+v", results)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
