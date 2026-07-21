package clisetup

import (
	"testing"
)

func TestBootstrapWarnings_ClaudeNoKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	warns := BootstrapWarnings("claude", "")
	if len(warns) == 0 {
		t.Error("expected warning when ANTHROPIC_API_KEY is missing for claude")
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
