package clisetup

import (
	"os"
	"path/filepath"
	"testing"
)

// noHostAuth stubs the host-auth probe so the developer's own `claude login`
// doesn't leak into the test.
func noHostAuth(t *testing.T) {
	t.Helper()
	orig := ClaudeHasHostAuth
	ClaudeHasHostAuth = func() bool { return false }
	t.Cleanup(func() { ClaudeHasHostAuth = orig })
}

// fileHostAuth exercises the real probe against an empty HOME, minus the
// keychain (which is per-user, not per-HOME).
func fileHostAuth(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
}

func TestBootstrapWarnings_ClaudeNoKey(t *testing.T) {
	noHostAuth(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	warns := BootstrapWarnings("claude", "")
	if len(warns) == 0 {
		t.Error("expected warning when ANTHROPIC_API_KEY is missing for claude")
	}
}

func TestBootstrapWarnings_ClaudeHostLogin(t *testing.T) {
	fileHostAuth(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"x@y"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if warns := BootstrapWarnings("claude", ""); len(warns) != 0 {
		t.Errorf("expected no warnings with host claude login, got: %v", warns)
	}
}

func TestBootstrapWarnings_ClaudeCredentialsFile(t *testing.T) {
	fileHostAuth(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if warns := BootstrapWarnings("claude", ""); len(warns) != 0 {
		t.Errorf("expected no warnings with .credentials.json present, got: %v", warns)
	}
}

func TestBootstrapWarnings_ClaudeWithKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	warns := BootstrapWarnings("claude", "")
	if len(warns) != 0 {
		t.Errorf("expected no warnings with ANTHROPIC_API_KEY set, got: %v", warns)
	}
}

func TestBootstrapWarnings_OpencodeNoKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	warns := BootstrapWarnings("opencode", "")
	if len(warns) == 0 {
		t.Error("expected warning when no API key is set for opencode")
	}
}

func TestBootstrapWarnings_OpencodeWithProviderKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	warns := BootstrapWarnings("opencode", "deepseek")
	if len(warns) != 0 {
		t.Errorf("expected no warnings with DEEPSEEK_API_KEY set, got: %v", warns)
	}
}

func TestBootstrapWarnings_PiNoKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	warns := BootstrapWarnings("pi", "anthropic")
	if len(warns) == 0 {
		t.Error("expected warning when ANTHROPIC_API_KEY is missing for pi/anthropic")
	}
}

func TestResolveCLI(t *testing.T) {
	for cli, want := range map[string]string{
		"":         "claude",
		"claude":   "claude",
		"codex":    "codex",
		"opencode": "opencode",
		"pi":       "pi",
		"vim":      "claude",
	} {
		if got := ResolveCLI(cli); got != want {
			t.Errorf("ResolveCLI(%q) = %q, want %q", cli, got, want)
		}
	}
}
