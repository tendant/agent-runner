package agenthome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdir runs the test from a temp runner directory.
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

func TestProvisionAndEnv(t *testing.T) {
	dir := chdir(t)

	if err := os.WriteFile("mcp.json",
		[]byte(`{"servers":{"maildirx":{"command":"/bin/maildirx","args":["mcp"],"env":{"MAIL_ROOT":"/mail"}}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(SkillsDir, "triage"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(SkillsDir, "triage", "SKILL.md"), []byte("# triage"), 0644); err != nil {
		t.Fatal(err)
	}

	results, err := Provision()
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	for _, res := range results {
		// claude requires the CLI; file-based clients must succeed.
		if res.CLI != "claude" && res.Err != nil {
			t.Errorf("%s: %v", res.CLI, res.Err)
		}
	}

	home := filepath.Join(dir, Dir)
	if data, err := os.ReadFile(filepath.Join(home, "codex", "config.toml")); err != nil ||
		!strings.Contains(string(data), "[mcp_servers.maildirx]") {
		t.Errorf("codex config not provisioned: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(home, "xdg", "opencode", "opencode.json")); err != nil ||
		!strings.Contains(string(data), "maildirx") {
		t.Errorf("opencode config not provisioned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "claude", "skills", "triage", "SKILL.md")); err != nil {
		t.Errorf("skill not synced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "pi", "agent", "skills", "triage", "SKILL.md")); err != nil {
		t.Errorf("skill not synced for pi: %v", err)
	}

	env, err := Env()
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude"),
		"CODEX_HOME=" + filepath.Join(home, "codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "xdg"),
		"XDG_DATA_HOME=" + filepath.Join(home, "xdg-data"),
		"PI_CODING_AGENT_DIR=" + filepath.Join(home, "pi", "agent"),
		"PI_CODING_AGENT_SESSION_DIR=" + filepath.Join(home, "pi", "sessions"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}

	// Idempotent.
	if _, err := Provision(); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
}

func TestProvisionSeedsPiConfig(t *testing.T) {
	dir := chdir(t)

	// Fake host home with pi provider config + credentials.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	piDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piDir, 0700); err != nil {
		t.Fatal(err)
	}
	models := `{"providers":{"ledgergate":{"baseUrl":"https://llm.example.com/v1","api":"openai-completions","apiKey":"$OPENAI_API_KEY","models":[{"id":"deepseek-v4-pro"}]}}}`
	if err := os.WriteFile(filepath.Join(piDir, "models.json"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piDir, "auth.json"), []byte(`{"anthropic":{"type":"api_key"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := Provision(); err != nil {
		t.Fatalf("provision: %v", err)
	}

	home := filepath.Join(dir, Dir)
	if data, err := os.ReadFile(filepath.Join(home, "pi", "agent", "models.json")); err != nil || string(data) != models {
		t.Errorf("pi models.json not seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "pi", "agent", "auth.json")); err != nil {
		t.Errorf("pi auth.json not seeded: %v", err)
	}

	// An existing copy is kept, not overwritten.
	if err := os.WriteFile(filepath.Join(home, "pi", "agent", "models.json"), []byte(`{"local":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(home, "pi", "agent", "models.json")); string(data) != `{"local":true}` {
		t.Error("existing agent-home models.json should not be overwritten")
	}
}

func TestProvisionWithoutDeclaration(t *testing.T) {
	chdir(t)
	results, err := Provision()
	if err != nil {
		t.Fatalf("provision without mcp.json should succeed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no MCP results, got %+v", results)
	}
	if _, err := os.Stat(filepath.Join(Dir, "claude")); err != nil {
		t.Error("agent-home dirs should exist regardless")
	}
}
