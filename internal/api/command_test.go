package api

import (
	"os"
	"os/exec"
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

	_, _, handled := c.Handle("hello world", nil)
	if handled {
		t.Error("expected unrecognised input to return handled=false")
	}
}

func TestCommander_Handle_BareCommandNormalization(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	for _, bare := range []string{"help", "Help", "HELP", "config", "bootstrap"} {
		reply, _, handled := c.Handle(bare, nil)
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
	_, _, handled := c.Handle("set AGENT_MODEL foo", nil)
	if handled {
		t.Error("'set' without slash should not be handled")
	}
}

// --- /help ---

func TestCommander_Help(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, handled := c.Handle("/help", nil)
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

	reply, _, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "opencode") {
		t.Errorf("expected opencode in config when CLI is empty, got:\n%s", reply)
	}
}

func TestCommander_Config_ShowsSetAPIKey(t *testing.T) {
	defer withTempCWD(t)()
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "DEEPSEEK_API_KEY") || !strings.Contains(reply, "set") {
		t.Errorf("expected DEEPSEEK_API_KEY: set when key is set, got:\n%s", reply)
	}
}

func TestCommander_Config_OmitsUnsetAPIKey(t *testing.T) {
	defer withTempCWD(t)()
	t.Setenv("DEEPSEEK_API_KEY", "")
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, _ := c.Handle("/config", nil)
	if strings.Contains(reply, "DEEPSEEK_API_KEY") {
		t.Errorf("unset API key should be omitted, got:\n%s", reply)
	}
}

func TestCommander_Config_FileStateMissing(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "agent.md") || !strings.Contains(reply, "missing") {
		t.Errorf("expected agent.md missing, got:\n%s", reply)
	}
	if !strings.Contains(reply, "prompt.md") || !strings.Contains(reply, "missing") {
		t.Errorf("expected prompt.md missing, got:\n%s", reply)
	}
}

func TestCommander_Config_FileStateExists(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)

	// Create the files where bootstrapPaths() points (memory/agent.md, memory/prompt.md)
	cwd, _ := os.Getwd()
	os.MkdirAll(filepath.Join(cwd, "memory"), 0755)
	os.WriteFile(filepath.Join(cwd, "memory", "agent.md"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(cwd, "memory", "prompt.md"), []byte("test"), 0644)

	c := NewCommander(env.handlers.config, env.handlers)
	reply, _, _ := c.Handle("/config", nil)

	if !strings.Contains(reply, "agent.md") || !strings.Contains(reply, "exists") {
		t.Errorf("expected agent.md exists, got:\n%s", reply)
	}
	if !strings.Contains(reply, "prompt.md") || !strings.Contains(reply, "exists") {
		t.Errorf("expected prompt.md exists, got:\n%s", reply)
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

	reply, _, _ := c.Handle("/config", nil)
	if !strings.Contains(reply, "ready") || strings.Contains(reply, "✓") {
		t.Errorf("expected ready ✗ when no API key, got:\n%s", reply)
	}
}

// --- /set ---

func TestCommander_Set_InvalidFormat(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	// "/set KEY" with no value — handled but returns usage error
	reply, _, handled := c.Handle("/set ONLYKEY", nil)
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

	reply, _, _ := c.Handle("/set AGENT_CLI claude", nil)
	if reply != "ok AGENT_CLI=claude" {
		t.Errorf("expected 'ok AGENT_CLI=claude', got: %s", reply)
	}
}

func TestCommander_Set_EqualsSyntax(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, _ := c.Handle("/set AGENT_CLI=codex", nil)
	if reply != "ok AGENT_CLI=codex" {
		t.Errorf("expected 'ok AGENT_CLI=codex', got: %s", reply)
	}
}

func TestCommander_Set_SensitiveKeyHidesValue(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	for _, key := range []string{"DEEPSEEK_API_KEY", "TELEGRAM_BOT_TOKEN", "MY_SECRET", "DB_PASSWORD"} {
		reply, _, _ := c.Handle("/set " + key + " super-secret-value", nil)
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

	reply, _, handled := c.Handle("/bootstrap", nil)
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
	reply, _, _ := c.Handle("/bootstrap", nil)

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
	reply, _, _ := c.Handle("/bootstrap force", nil)

	if !strings.Contains(reply, "created") || !strings.Contains(reply, "agent.md") {
		t.Errorf("expected 'created agent.md' with force, got:\n%s", reply)
	}
}

// --- /set-agent and /set-prompt ---

func TestCommander_SetAgent_EmptyContent(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, handled := c.Handle("/set-agent", nil)
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

	reply, _, _ := c.Handle("/set-agent you are a helpful agent", nil)
	if !strings.Contains(reply, "ok wrote agent.md") {
		t.Errorf("expected 'ok wrote agent.md', got: %s", reply)
	}

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "memory", "agent.md"))
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

	reply, _, _ := c.Handle("/set-prompt do the task step by step", nil)
	if !strings.Contains(reply, "ok wrote prompt.md") {
		t.Errorf("expected 'ok wrote prompt.md', got: %s", reply)
	}

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "memory", "prompt.md"))
	if err != nil {
		t.Fatalf("expected prompt.md to be created: %v", err)
	}
	if !strings.Contains(string(data), "do the task step by step") {
		t.Errorf("unexpected prompt.md content: %s", string(data))
	}
}

// --- /migrate ---

func TestCommander_Migrate_NothingToMigrate(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, handled := c.Handle("/migrate", nil)
	if !handled {
		t.Fatal("expected /migrate to be handled")
	}
	if !strings.Contains(reply, "nothing to migrate") {
		t.Errorf("expected 'nothing to migrate', got: %s", reply)
	}
}

func TestCommander_Migrate_MigratesMemoryMD(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	memDir := env.handlers.config.MemoryDir
	os.MkdirAll(memDir, 0755)
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("## User Preferences\n- prefers Go\n\n## Project Context\nsome project info"), 0644)

	reply, _, handled := c.Handle("/migrate", nil)
	if !handled {
		t.Fatal("expected /migrate to be handled")
	}
	if !strings.Contains(reply, "MEMORY.md") {
		t.Errorf("expected MEMORY.md in reply, got: %s", reply)
	}
	// Source file renamed to .migrated
	if _, err := os.Stat(filepath.Join(memDir, "MEMORY.md.migrated")); err != nil {
		t.Error("expected MEMORY.md to be renamed to MEMORY.md.migrated")
	}
	// Target files created
	prefs, _ := os.ReadFile(filepath.Join(memDir, "user_preferences.md"))
	if !strings.Contains(string(prefs), "prefers Go") {
		t.Errorf("expected user_preferences.md to contain migrated content, got: %s", prefs)
	}
	summary, _ := os.ReadFile(filepath.Join(memDir, "project_summary.md"))
	if !strings.Contains(string(summary), "some project info") {
		t.Errorf("expected project_summary.md to contain migrated content, got: %s", summary)
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

// --- /memory ---

func newMemoryCommander(t *testing.T) (*Commander, string) {
	t.Helper()
	env := setupTestEnv(t)
	memDir := filepath.Join(t.TempDir(), "memory")
	env.handlers.config.MemoryDir = memDir
	return NewCommander(env.handlers.config, env.handlers), memDir
}

func TestCommander_Memory_NoSubcommand_ShowsStatus(t *testing.T) {
	c, _ := newMemoryCommander(t)
	reply, _, handled := c.Handle("/memory", nil)
	if !handled {
		t.Fatal("expected /memory to be handled")
	}
	if !strings.Contains(reply, "Memory") {
		t.Errorf("expected status output, got: %s", reply)
	}
}

func TestCommander_Memory_Status(t *testing.T) {
	c, _ := newMemoryCommander(t)
	reply, _, handled := c.Handle("/memory status", nil)
	if !handled {
		t.Fatal("expected /memory status to be handled")
	}
	if !strings.Contains(reply, "Memory") {
		t.Errorf("expected memory status output, got: %s", reply)
	}
}

func TestCommander_Memory_Git_MissingRemote(t *testing.T) {
	c, _ := newMemoryCommander(t)
	reply, _, handled := c.Handle("/memory git", nil)
	if !handled {
		t.Fatal("expected /memory git to be handled")
	}
	if !strings.Contains(reply, "error") || !strings.Contains(reply, "usage") {
		t.Errorf("expected usage error, got: %s", reply)
	}
}

func TestCommander_Memory_Git_InitialisesRepo(t *testing.T) {
	c, memDir := newMemoryCommander(t)
	reply, _, handled := c.Handle("/memory git file:///"+t.TempDir(), nil)
	if !handled {
		t.Fatal("expected /memory git to be handled")
	}
	// Should have initialised git
	if _, err := os.Stat(filepath.Join(memDir, ".git")); os.IsNotExist(err) {
		t.Error("expected .git to be created in memory dir")
	}
	if !strings.Contains(reply, "initialised") && !strings.Contains(reply, "remote") {
		t.Errorf("expected init confirmation, got: %s", reply)
	}
}

func TestCommander_Memory_Git_SameRemote_NoOp(t *testing.T) {
	c, _ := newMemoryCommander(t)
	remote := "file:///" + t.TempDir()
	c.Handle("/memory git "+remote, nil) // first call: init
	reply, _, _ := c.Handle("/memory git "+remote, nil) // second call: same remote
	if !strings.Contains(reply, "already configured") {
		t.Errorf("expected 'already configured', got: %s", reply)
	}
}

func TestCommander_Memory_Git_UpdatesRemote(t *testing.T) {
	c, _ := newMemoryCommander(t)
	remote1 := "file:///" + t.TempDir()
	remote2 := "file:///" + t.TempDir()
	c.Handle("/memory git "+remote1, nil)
	reply, _, _ := c.Handle("/memory git "+remote2, nil)
	if !strings.Contains(reply, "remote updated") {
		t.Errorf("expected 'remote updated', got: %s", reply)
	}
}

func TestCommander_Memory_Keygen(t *testing.T) {
	c, _ := newMemoryCommander(t)
	reply, _, handled := c.Handle("/memory keygen", nil)
	if !handled {
		t.Fatal("expected /memory keygen to be handled")
	}
	if !strings.Contains(reply, "ssh-ed25519") {
		t.Errorf("expected public key in output, got: %s", reply)
	}
}

func TestCommander_Memory_Keygen_ExistingKey(t *testing.T) {
	c, memDir := newMemoryCommander(t)
	// Generate key first
	c.Handle("/memory keygen", nil)
	// Call again — should print existing key, not regenerate
	reply, _, _ := c.Handle("/memory keygen", nil)
	if !strings.Contains(reply, "already exists") {
		t.Errorf("expected 'already exists', got: %s", reply)
	}
	if !strings.Contains(reply, "ssh-ed25519") {
		t.Errorf("expected public key content in output, got: %s", reply)
	}
	_ = memDir
}

func TestCommander_Memory_Pubkey_NoKey(t *testing.T) {
	c, _ := newMemoryCommander(t)
	reply, _, handled := c.Handle("/memory pubkey", nil)
	if !handled {
		t.Fatal("expected /memory pubkey to be handled")
	}
	if !strings.Contains(reply, "no public key") {
		t.Errorf("expected 'no public key' message, got: %s", reply)
	}
}

func TestCommander_Memory_Pubkey_AfterKeygen(t *testing.T) {
	c, _ := newMemoryCommander(t)
	c.Handle("/memory keygen", nil)
	reply, _, _ := c.Handle("/memory pubkey", nil)
	if !strings.Contains(reply, "ssh-ed25519") {
		t.Errorf("expected public key content, got: %s", reply)
	}
}

func TestCommander_Memory_UnknownSubcommand(t *testing.T) {
	c, _ := newMemoryCommander(t)
	reply, _, handled := c.Handle("/memory bogus", nil)
	if !handled {
		t.Fatal("expected /memory bogus to be handled")
	}
	if !strings.Contains(reply, "unknown subcommand") {
		t.Errorf("expected unknown subcommand error, got: %s", reply)
	}
}

func TestCommander_Memory_Push_NotGitRepo(t *testing.T) {
	c, _ := newMemoryCommander(t)
	// Memory dir not initialised — push must return a clear error.
	reply, _, handled := c.Handle("/memory push", nil)
	if !handled {
		t.Fatal("expected /memory push to be handled")
	}
	if !strings.Contains(reply, "error") || !strings.Contains(reply, "not a git repo") {
		t.Errorf("expected not-a-git-repo error, got: %s", reply)
	}
}

func TestCommander_Memory_Push_Success(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	defer withTempCWD(t)()
	c, memDir := newMemoryCommander(t)

	bare := makeBarRepo(t, filepath.Join(t.TempDir(), "mem.git"))
	c.Handle("/memory git "+bare, nil)

	// Create a file so there is something to commit and push.
	if err := os.WriteFile(filepath.Join(memDir, "note.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reply, _, _ := c.Handle("/memory push", nil)
	if !strings.Contains(reply, "memory pushed") {
		t.Errorf("expected push success, got: %s", reply)
	}
}

func TestCommander_Memory_Push_Idempotent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	defer withTempCWD(t)()
	c, memDir := newMemoryCommander(t)

	bare := makeBarRepo(t, filepath.Join(t.TempDir(), "mem.git"))
	c.Handle("/memory git "+bare, nil)
	if err := os.WriteFile(filepath.Join(memDir, "note.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c.Handle("/memory push", nil) // first push

	// Second push with no changes must not return an error.
	reply, _, _ := c.Handle("/memory push", nil)
	if strings.HasPrefix(reply, "error") {
		t.Errorf("expected no error on idempotent push, got: %s", reply)
	}
	if !strings.Contains(reply, "memory pushed") {
		t.Errorf("expected push success on second push, got: %s", reply)
	}
}

func TestCommander_Memory_Pull_NotGitRepo(t *testing.T) {
	c, _ := newMemoryCommander(t)
	// Memory dir not initialised — pull must return a clear error.
	reply, _, handled := c.Handle("/memory pull", nil)
	if !handled {
		t.Fatal("expected /memory pull to be handled")
	}
	if !strings.Contains(reply, "error") || !strings.Contains(reply, "not a git repo") {
		t.Errorf("expected not-a-git-repo error, got: %s", reply)
	}
}

// --- /status ---

func TestCommander_Status_ShowsFields(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, handled := c.Handle("/status", nil)
	if !handled {
		t.Fatal("expected /status to be handled")
	}
	for _, field := range []string{"agent:", "queued:", "cli:", "ready:"} {
		if !strings.Contains(reply, field) {
			t.Errorf("expected %q field in status, got:\n%s", field, reply)
		}
	}
}

func TestCommander_Status_IdleWhenNoSessions(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, _ := c.Handle("/status", nil)
	if !strings.Contains(reply, "idle") {
		t.Errorf("expected idle status with no sessions, got:\n%s", reply)
	}
}

func TestCommander_Status_ReadyFalseWhenNoAPIKey(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
	env := setupTestEnv(t)
	env.handlers.config.Agent.CLI = "claude"
	env.handlers.config.Agent.Provider = ""
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, _ := c.Handle("/status", nil)
	if !strings.Contains(reply, "ready:") || strings.Contains(reply, "ready: ✓") {
		t.Errorf("expected ready ✗ when no API key, got:\n%s", reply)
	}
}

// --- /auth ---

func TestCommander_Auth_RESTCallReturnsError(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	// send=nil simulates a REST caller with no chat channel.
	reply, _, handled := c.Handle("/auth claude", nil)
	if !handled {
		t.Fatal("expected /auth to be handled")
	}
	if !strings.Contains(reply, "error") || !strings.Contains(reply, "only available via chat") {
		t.Errorf("expected chat-only error, got: %s", reply)
	}
}

func TestCommander_AuthCancel_NoFlow(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, handled := c.Handle("/auth cancel", nil)
	if !handled {
		t.Fatal("expected /auth cancel to be handled")
	}
	if reply != "no auth flow is running" {
		t.Errorf("expected 'no auth flow is running', got: %s", reply)
	}
}

func TestCommander_Auth_OpencodeNotSupported(t *testing.T) {
	env := setupTestEnv(t)
	env.handlers.config.Agent.CLI = "opencode"
	c := NewCommander(env.handlers.config, env.handlers)

	var sent []string
	send := func(msg string) { sent = append(sent, msg) }

	// /auth with no arg falls back to configured CLI (opencode), which isn't supported.
	reply, _, handled := c.Handle("/auth", send)
	if !handled {
		t.Fatal("expected /auth to be handled")
	}
	if !strings.Contains(reply, "error") {
		t.Errorf("expected error for opencode auth, got: %s", reply)
	}
}

// --- parseSystemUser ---

func TestParseSystemUser_BothSections(t *testing.T) {
	body := "System: be a helpful agent\nUser: do the task"
	system, user := parseSystemUser(body)
	if system != "be a helpful agent" {
		t.Errorf("expected system=%q, got %q", "be a helpful agent", system)
	}
	if user != "do the task" {
		t.Errorf("expected user=%q, got %q", "do the task", user)
	}
}

func TestParseSystemUser_OnlySystem(t *testing.T) {
	body := "System: be a helpful agent"
	system, user := parseSystemUser(body)
	if system != "be a helpful agent" {
		t.Errorf("expected system=%q, got %q", "be a helpful agent", system)
	}
	if user != "" {
		t.Errorf("expected user empty, got %q", user)
	}
}

func TestParseSystemUser_OnlyUser(t *testing.T) {
	body := "User: do the task"
	system, user := parseSystemUser(body)
	if system != "" {
		t.Errorf("expected system empty, got %q", system)
	}
	if user != "do the task" {
		t.Errorf("expected user=%q, got %q", "do the task", user)
	}
}

func TestParseSystemUser_NoSections_PlainText(t *testing.T) {
	body := "just some plain text"
	system, user := parseSystemUser(body)
	if system != "" {
		t.Errorf("expected system empty for plain text, got %q", system)
	}
	if user != "" {
		t.Errorf("expected user empty for plain text, got %q", user)
	}
}

func TestParseSystemUser_EmptyBody(t *testing.T) {
	system, user := parseSystemUser("")
	if system != "" {
		t.Errorf("expected system empty for empty body, got %q", system)
	}
	if user != "" {
		t.Errorf("expected user empty for empty body, got %q", user)
	}
}

func TestParseSystemUser_MultilineBothSections(t *testing.T) {
	body := "System: first system line\nsecond system line\nUser: first user line\nsecond user line"
	system, user := parseSystemUser(body)
	if !strings.Contains(system, "first system line") || !strings.Contains(system, "second system line") {
		t.Errorf("expected both system lines, got %q", system)
	}
	if !strings.Contains(user, "first user line") || !strings.Contains(user, "second user line") {
		t.Errorf("expected both user lines, got %q", user)
	}
}

// --- handleUpdatePrompt ---

func TestHandleUpdatePrompt_PlainBody(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, sid, handled := c.Handle("Update prompt: follow the rules carefully", nil)
	if !handled {
		t.Fatal("expected Update prompt: to be handled")
	}
	if sid != "" {
		t.Errorf("expected no session for plain body, got %q", sid)
	}
	if !strings.Contains(reply, "ok wrote prompt.md") {
		t.Errorf("expected ok reply, got: %s", reply)
	}

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "memory", "prompt.md"))
	if err != nil {
		t.Fatalf("expected prompt.md to be created: %v", err)
	}
	if !strings.Contains(string(data), "follow the rules carefully") {
		t.Errorf("expected body in prompt.md, got:\n%s", data)
	}
}

func TestHandleUpdatePrompt_EmptyBody(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, handled := c.Handle("Update prompt:", nil)
	if !handled {
		t.Fatal("expected Update prompt: to be handled")
	}
	if !strings.HasPrefix(reply, "error:") {
		t.Errorf("expected error reply for empty body, got: %s", reply)
	}
}

func TestHandleUpdatePrompt_SystemSection(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	body := "Update prompt:\nSystem: always commit at the end"
	reply, sid, handled := c.Handle(body, nil)
	if !handled {
		t.Fatal("expected Update prompt: to be handled")
	}
	if sid != "" {
		t.Errorf("expected no session for system-only body, got %q", sid)
	}
	if !strings.Contains(reply, "ok wrote prompt.md") {
		t.Errorf("expected ok reply, got: %s", reply)
	}

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "memory", "prompt.md"))
	if err != nil {
		t.Fatalf("expected prompt.md to be created: %v", err)
	}
	if !strings.Contains(string(data), "always commit at the end") {
		t.Errorf("expected system content in prompt.md, got:\n%s", data)
	}
}

func TestHandleUpdatePrompt_SystemAndUser(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	// Disable planner so the background goroutine doesn't panic on nil plannerClient.
	env.handlers.config.Agent.PlannerEnabled = false
	c := NewCommander(env.handlers.config, env.handlers)

	body := "Update prompt:\nSystem: always commit at the end\nUser: fix the tests"
	reply, sid, handled := c.Handle(body, nil)
	if !handled {
		t.Fatal("expected Update prompt: to be handled")
	}
	if sid == "" {
		t.Error("expected a session ID when User: section is present")
	}
	if !strings.Contains(reply, "ok wrote prompt.md") {
		t.Errorf("expected ok reply mentioning prompt.md, got: %s", reply)
	}
	if !strings.Contains(reply, sid) {
		t.Errorf("expected reply to contain session ID %q, got: %s", sid, reply)
	}

	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "memory", "prompt.md"))
	if err != nil {
		t.Fatalf("expected prompt.md to be created: %v", err)
	}
	if !strings.Contains(string(data), "always commit at the end") {
		t.Errorf("expected system content in prompt.md, got:\n%s", data)
	}
}

func TestHandleUpdatePrompt_SlashVariant(t *testing.T) {
	defer withTempCWD(t)()
	env := setupTestEnv(t)
	c := NewCommander(env.handlers.config, env.handlers)

	reply, _, handled := c.Handle("/update-prompt: keep responses brief", nil)
	if !handled {
		t.Fatal("expected /update-prompt: to be handled")
	}
	if !strings.Contains(reply, "ok wrote prompt.md") {
		t.Errorf("expected ok reply, got: %s", reply)
	}
}
