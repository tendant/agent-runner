package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

)

func withTempCWD(t *testing.T) (restore func()) {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chdir(orig) } //nolint:errcheck
}

// --- Handle() dispatch ---

func TestCommander_Handle_UnknownInput(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	_, handled := c.Handle("hello world", nil)
	if handled {
		t.Error("expected unrecognised input to return handled=false")
	}
}

func TestCommander_Handle_BareCommandNormalization(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	for _, bare := range []string{"help", "Help", "HELP", "config", "bootstrap"} {
		reply, handled := c.Handle(bare, nil)
		if !handled {
			t.Errorf("bare command %q should be handled", bare)
		}
		if reply == "" {
			t.Errorf("bare command %q returned empty reply", bare)
		}
	}
}

func TestCommander_Handle_MultiWordRequiresSlash(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	// "set" alone should NOT be handled (multi-word command, needs slash)
	_, handled := c.Handle("set AGENT_MODEL foo", nil)
	if handled {
		t.Error("'set' without slash should not be handled")
	}
}

// --- /help ---

func TestCommander_Help(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, handled := c.Handle("/help", nil)
	if !handled {
		t.Fatal("expected /help to be handled")
	}
	if !strings.Contains(reply, "/config") || !strings.Contains(reply, "/set") {
		t.Errorf("help text missing expected commands, got: %s", reply)
	}
}

// --- /config ---

func TestCommander_Config_ShowsCLIDefault(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	env.handlers.config.Agent.CLI = ""
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "cli=opencode") {
		t.Errorf("expected cli=opencode when CLI is empty, got:\n%s", reply)
	}
}

func TestCommander_Config_ShowsSetAPIKey(t *testing.T) {
	defer withTempCWD(t)()
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "DEEPSEEK_API_KEY=set") {
		t.Errorf("expected DEEPSEEK_API_KEY=set when key is set, got:\n%s", reply)
	}
}

func TestCommander_Config_OmitsUnsetAPIKey(t *testing.T) {
	defer withTempCWD(t)()
	t.Setenv("DEEPSEEK_API_KEY", "")
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/config", nil)
	if strings.Contains(reply, "DEEPSEEK_API_KEY") {
		t.Errorf("unset API key should be omitted, got:\n%s", reply)
	}
}

func TestCommander_Config_FileStateMissing(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "agent.md=missing") {
		t.Errorf("expected agent.md=missing, got:\n%s", reply)
	}
	if !strings.Contains(reply, "prompt.md=missing") {
		t.Errorf("expected prompt.md=missing, got:\n%s", reply)
	}
}

func TestCommander_Config_FileStateExists(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)

	// Create the files where bootstrapPaths() points (./agent.md, ./prompt.md by default)
	cwd, _ := os.Getwd()
	os.WriteFile(filepath.Join(cwd, "agent.md"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(cwd, "prompt.md"), []byte("test"), 0644)

	c := NewCommander(env.handlers.config, env.handlers)
	reply, _ := c.Handle("/config", nil)

	if !strings.Contains(reply, "agent.md=exists") {
		t.Errorf("expected agent.md=exists, got:\n%s", reply)
	}
	if !strings.Contains(reply, "prompt.md=exists") {
		t.Errorf("expected prompt.md=exists, got:\n%s", reply)
	}
}

func TestCommander_Config_ReadyFalseWhenNoAPIKey(t *testing.T) {
	defer withTempCWD(t)()
	// Ensure no provider keys set
	for _, k := range []string{"ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
	env := setupTestEnv(t)
	env.handlers.config.Agent.CLI = "claude"
	env.handlers.config.Agent.Provider = ""
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "ready=false") {
		t.Errorf("expected ready=false when no API key, got:\n%s", reply)
	}
}

// --- /set ---

func TestCommander_Set_InvalidFormat(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	// "/set KEY" with no value — handled but returns usage error
	reply, handled := c.Handle("/set ONLYKEY", nil)
	if !handled {
		t.Fatal("expected /set ONLYKEY to be handled")
	}
	if !strings.HasPrefix(reply, "error:") {
		t.Errorf("expected error reply for /set with no value, got: %s", reply)
	}
}

func TestCommander_Set_SpaceSyntax(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/set AGENT_CLI claude", nil)
	if reply != "ok AGENT_CLI=claude" {
		t.Errorf("expected 'ok AGENT_CLI=claude', got: %s", reply)
	}
}

func TestCommander_Set_EqualsSyntax(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/set AGENT_CLI=codex", nil)
	if reply != "ok AGENT_CLI=codex" {
		t.Errorf("expected 'ok AGENT_CLI=codex', got: %s", reply)
	}
}

func TestCommander_Set_SensitiveKeyHidesValue(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	for _, key := range []string{"DEEPSEEK_API_KEY", "TELEGRAM_BOT_TOKEN", "MY_SECRET", "DB_PASSWORD"} {
		reply, _ := c.Handle("/set " + key + " super-secret-value", nil)
		if reply != "ok "+key {
			t.Errorf("sensitive key %s: expected 'ok %s', got: %s", key, key, reply)
		}
		if strings.Contains(reply, "super-secret-value") {
			t.Errorf("sensitive key %s: value must not appear in reply", key)
		}
	}
}

func TestCommander_Set_WritesEnvLocal(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	c.Handle("/set MY_VAR hello", nil)

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, ".env.local"))
	if err != nil {
		t.Fatalf("expected .env.local to be created: %v", err)
	}
	if !strings.Contains(string(data), "MY_VAR=hello") {
		t.Errorf("expected MY_VAR=hello in .env.local, got:\n%s", string(data))
	}
}

func TestCommander_Set_LiveConfigUpdate_AgentCLI(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	c.Handle("/set AGENT_CLI claude", nil)
	if env.handlers.config.Agent.CLI != "claude" {
		t.Errorf("expected Agent.CLI=claude, got: %s", env.handlers.config.Agent.CLI)
	}
}

func TestCommander_Set_LiveConfigUpdate_AgentModel(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	c.Handle("/set AGENT_MODEL my-model", nil)
	if env.handlers.config.Agent.Model != "my-model" {
		t.Errorf("expected Agent.Model=my-model, got: %s", env.handlers.config.Agent.Model)
	}
}

func TestCommander_Set_LiveConfigUpdate_AgentMaxTurns(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	c.Handle("/set AGENT_MAX_TURNS 99", nil)
	if env.handlers.config.Agent.MaxTurns != 99 {
		t.Errorf("expected Agent.MaxTurns=99, got: %d", env.handlers.config.Agent.MaxTurns)
	}
}

// --- /bootstrap ---

func TestCommander_Bootstrap_CreatesFiles(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	// Ensure API key so ready=true
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	env.handlers.config.Agent.CLI = "opencode"
	env.handlers.config.Agent.Provider = "deepseek"
	c := NewCommander(env.handlers.config, env.handlers)

	reply, handled := c.Handle("/bootstrap", nil)
	if !handled {
		t.Fatal("expected /bootstrap to be handled")
	}
	if !strings.Contains(reply, "created") || !strings.Contains(reply, "agent.md") {
		t.Errorf("expected 'created agent.md', got:\n%s", reply)
	}
	if !strings.Contains(reply, "created") || !strings.Contains(reply, "prompt.md") {
		t.Errorf("expected 'created prompt.md', got:\n%s", reply)
	}
	if !strings.Contains(reply, "ready=true") {
		t.Errorf("expected ready=true, got:\n%s", reply)
	}
}

func TestCommander_Bootstrap_SkipsExisting(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	env.handlers.config.Agent.CLI = "opencode"
	env.handlers.config.Agent.Provider = "deepseek"
	c := NewCommander(env.handlers.config, env.handlers)

	// Create first
	c.Handle("/bootstrap", nil)
	// Run again without force
	reply, _ := c.Handle("/bootstrap", nil)

	if !strings.Contains(reply, "skipped") || !strings.Contains(reply, "agent.md") {
		t.Errorf("expected 'skipped agent.md', got:\n%s", reply)
	}
	if !strings.Contains(reply, "skipped") || !strings.Contains(reply, "prompt.md") {
		t.Errorf("expected 'skipped prompt.md', got:\n%s", reply)
	}
}

func TestCommander_Bootstrap_ForceOverwrites(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	env.handlers.config.Agent.CLI = "opencode"
	env.handlers.config.Agent.Provider = "deepseek"
	c := NewCommander(env.handlers.config, env.handlers)

	c.Handle("/bootstrap", nil)
	reply, _ := c.Handle("/bootstrap force", nil)

	if !strings.Contains(reply, "created") || !strings.Contains(reply, "agent.md") {
		t.Errorf("expected 'created agent.md' with force, got:\n%s", reply)
	}
}

// --- /set-agent and /set-prompt ---

func TestCommander_SetAgent_EmptyContent(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, handled := c.Handle("/set-agent", nil)
	if !handled {
		t.Fatal("expected /set-agent to be handled")
	}
	if !strings.HasPrefix(reply, "error:") {
		t.Errorf("expected error for empty content, got: %s", reply)
	}
}

func TestCommander_SetAgent_WritesFile(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/set-agent you are a helpful agent", nil)
	if !strings.Contains(reply, "ok wrote agent.md") {
		t.Errorf("expected 'ok wrote agent.md', got: %s", reply)
	}

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "agent.md"))
	if err != nil {
		t.Fatalf("expected agent.md to be created: %v", err)
	}
	if !strings.Contains(string(data), "you are a helpful agent") {
		t.Errorf("unexpected agent.md content: %s", string(data))
	}
}

func TestCommander_SetPrompt_WritesFile(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _ := c.Handle("/set-prompt do the task step by step", nil)
	if !strings.Contains(reply, "ok wrote prompt.md") {
		t.Errorf("expected 'ok wrote prompt.md', got: %s", reply)
	}

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "prompt.md"))
	if err != nil {
		t.Fatalf("expected prompt.md to be created: %v", err)
	}
	if !strings.Contains(string(data), "do the task step by step") {
		t.Errorf("unexpected prompt.md content: %s", string(data))
	}
}

// --- parseSetArgs ---

func TestParseSetArgs_SpaceSyntax(t *testing.T) {
	key, val, ok := parseSetArgs("MY_KEY my_value")
	if !ok || key != "MY_KEY" || val != "my_value" {
		t.Errorf("expected MY_KEY/my_value/true, got %q/%q/%v", key, val, ok)
	}
}

func TestParseSetArgs_EqualsSyntax(t *testing.T) {
	key, val, ok := parseSetArgs("MY_KEY=my_value")
	if !ok || key != "MY_KEY" || val != "my_value" {
		t.Errorf("expected MY_KEY/my_value/true, got %q/%q/%v", key, val, ok)
	}
}

func TestParseSetArgs_EqualsValueWithEquals(t *testing.T) {
	// Value itself contains '=' — only first = is the separator
	key, val, ok := parseSetArgs("MY_KEY=a=b")
	if !ok || key != "MY_KEY" || val != "a=b" {
		t.Errorf("expected MY_KEY/a=b/true, got %q/%q/%v", key, val, ok)
	}
}

func TestParseSetArgs_Invalid(t *testing.T) {
	_, _, ok := parseSetArgs("NOKEYVALUE")
	if ok {
		t.Error("expected ok=false for bare word with no value")
	}
}

func TestParseSetArgs_Empty(t *testing.T) {
	_, _, ok := parseSetArgs("")
	if ok {
		t.Error("expected ok=false for empty string")
	}
}

// --- isSensitiveKey ---

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"DEEPSEEK_API_KEY", "ANTHROPIC_API_KEY", "MY_API_KEY",
		"TELEGRAM_BOT_TOKEN", "STREAM_BOT_TOKEN",
		"MY_SECRET", "DB_SECRET",
		"DB_PASSWORD", "ADMIN_PASSWORD",
	}
	for _, k := range sensitive {
		if !isSensitiveKey(k) {
			t.Errorf("expected %s to be sensitive", k)
		}
	}
}

func TestIsSensitiveKey_NonSensitive(t *testing.T) {
	nonsensitive := []string{
		"AGENT_CLI", "AGENT_MODEL", "AGENT_PROVIDER",
		"BIND", "LOGS_ROOT", "MAX_RUNTIME_SECONDS",
	}
	for _, k := range nonsensitive {
		if isSensitiveKey(k) {
			t.Errorf("expected %s to be non-sensitive", k)
		}
	}
}
