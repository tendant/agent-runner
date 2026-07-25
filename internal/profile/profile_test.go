package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdir runs the test from a temp directory so Root resolves inside it.
func chdir(t *testing.T) string {
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
	// macOS temp dirs are symlinked (/var -> /private/var); return the
	// resolved path so expectations match filepath.Abs results.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestEnvEmptyNameIsLegacy(t *testing.T) {
	env, err := Env("")
	if err != nil || env != nil {
		t.Fatalf("empty profile must be nil env: %v, %v", env, err)
	}
}

func TestEnvMissingProfile(t *testing.T) {
	chdir(t)
	if _, err := Env("nope"); err == nil {
		t.Fatal("missing profile should error")
	}
}

func TestProvisionAndEnv(t *testing.T) {
	dir := chdir(t)

	pdir := filepath.Join(dir, Root, "work")
	if err := os.MkdirAll(pdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "profile.env"),
		[]byte("MAILDIRX_MCP_MODE=read-only\nSEED_AUTH=false\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Declare one MCP server; claude registration is exercised via the
	// mcpsetup stub in its own tests — here we only use codex/opencode paths,
	// so restrict the declaration check to the file outputs.
	if err := os.WriteFile(filepath.Join(pdir, "mcp.json"),
		[]byte(`{"servers":{"maildirx":{"command":"/bin/maildirx","args":["mcp"],"env":{"MAIL_ROOT":"/mail"}}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	// A skill to sync.
	if err := os.MkdirAll(filepath.Join(pdir, "skills", "triage"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "skills", "triage", "SKILL.md"), []byte("# triage"), 0644); err != nil {
		t.Fatal(err)
	}

	results, err := Provision("work")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Codex and opencode configs land inside the profile.
	codex := filepath.Join(pdir, "codex", "config.toml")
	if data, err := os.ReadFile(codex); err != nil || !strings.Contains(string(data), "[mcp_servers.maildirx]") {
		t.Errorf("codex config not provisioned: %v", err)
	}
	opencode := filepath.Join(pdir, "xdg", "opencode", "opencode.json")
	if data, err := os.ReadFile(opencode); err != nil || !strings.Contains(string(data), "maildirx") {
		t.Errorf("opencode config not provisioned: %v", err)
	}
	// The skill is synced into the claude config dir.
	if _, err := os.Stat(filepath.Join(pdir, "claude", "skills", "triage", "SKILL.md")); err != nil {
		t.Errorf("skill not synced: %v", err)
	}
	// Result actions for the file-based clients succeed.
	for _, res := range results {
		if res.CLI != "claude" && res.Err != nil {
			t.Errorf("%s: %v", res.CLI, res.Err)
		}
	}

	// Env: redirections + profile.env, minus SEED_AUTH.
	env, err := Env("work")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"CLAUDE_CONFIG_DIR=" + filepath.Join(pdir, "claude"),
		"CODEX_HOME=" + filepath.Join(pdir, "codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(pdir, "xdg"),
		"XDG_DATA_HOME=" + filepath.Join(pdir, "xdg-data"),
		"MAILDIRX_MCP_MODE=read-only",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "SEED_AUTH") {
		t.Error("SEED_AUTH leaked into runtime env")
	}

	// Idempotent.
	if _, err := Provision("work"); err != nil {
		t.Fatalf("re-provision: %v", err)
	}

	// List sees it.
	names := List()
	if len(names) != 1 || names[0] != "work" {
		t.Errorf("list: %v", names)
	}
}
