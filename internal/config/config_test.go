package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	data := defaultDataDir()
	if cfg.RepoCacheRoot != filepath.Join(data, "repo-cache") {
		t.Errorf("expected RepoCacheRoot under data dir, got %s", cfg.RepoCacheRoot)
	}
	if cfg.LogsRoot != filepath.Join(data, "logs") {
		t.Errorf("expected LogsRoot under data dir, got %s", cfg.LogsRoot)
	}
	if cfg.TmpRoot != filepath.Join(data, "tmp") {
		t.Errorf("expected TmpRoot under data dir, got %s", cfg.TmpRoot)
	}
	// Default data dir is ~/.agent-runner (absolute), falling back to "." only
	// when the home directory cannot be resolved.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if !strings.HasPrefix(cfg.RepoCacheRoot, filepath.Join(home, ".agent-runner")) {
			t.Errorf("expected RepoCacheRoot under ~/.agent-runner, got %s", cfg.RepoCacheRoot)
		}
	}
	if cfg.MaxRuntimeSeconds != 300 {
		t.Errorf("expected MaxRuntimeSeconds 300, got %d", cfg.MaxRuntimeSeconds)
	}
	if cfg.MaxConcurrentJobs != 5 {
		t.Errorf("expected MaxConcurrentJobs 5, got %d", cfg.MaxConcurrentJobs)
	}
	if cfg.API.Bind != "127.0.0.1:8080" {
		t.Errorf("expected Bind 127.0.0.1:8080, got %s", cfg.API.Bind)
	}
	if cfg.Agent.Author != "claude-agent" {
		t.Errorf("expected Agent.Author claude-agent, got %s", cfg.Agent.Author)
	}
	if cfg.Agent.CommitPrefix != "[agent]" {
		t.Errorf("expected Agent.CommitPrefix [agent], got %s", cfg.Agent.CommitPrefix)
	}
	if cfg.Agent.MaxTurns != 50 {
		t.Errorf("expected Agent.MaxTurns 50, got %d", cfg.Agent.MaxTurns)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config should pass validation, got: %v", err)
	}
}

func TestLoadFromEnv_ExplicitDataDirWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RepoCacheRoot != filepath.Join(dir, "repo-cache") {
		t.Errorf("explicit DATA_DIR should root state dirs, got %s", cfg.RepoCacheRoot)
	}
}

func TestLoadFromEnv_LegacyLayoutFallsBackToCWD(t *testing.T) {
	defer chtempdir(t)()
	t.Setenv("DATA_DIR", "")
	// Simulate the old CWD-based layout: a non-empty repo-cache/ in CWD.
	if err := os.MkdirAll("repo-cache/somerepo", 0755); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RepoCacheRoot != filepath.Join(".", "repo-cache") {
		t.Errorf("legacy layout should keep DATA_DIR=. for this run, got %s", cfg.RepoCacheRoot)
	}
}

func TestLoadFromEnv_CleanDirDefaultsToHome(t *testing.T) {
	defer chtempdir(t)()
	t.Setenv("DATA_DIR", "")
	t.Setenv("INSTANCE", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	home, herr := os.UserHomeDir()
	if herr != nil || home == "" {
		t.Skip("no home dir available")
	}
	if !strings.HasPrefix(cfg.RepoCacheRoot, filepath.Join(home, ".agent-runner")) {
		t.Errorf("clean CWD should default DATA_DIR to ~/.agent-runner, got %s", cfg.RepoCacheRoot)
	}
}

// chtempdir switches the CWD to a temp dir for the duration of a test.
func chtempdir(t *testing.T) (restore func()) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chdir(orig) } //nolint:errcheck
}

func TestLoad_OpenCodeModelDefaults(t *testing.T) {
	// opencode (the default CLI) should get deepseek model defaults from Load().
	cfg, _ := LoadFromEnv()
	if cfg.Agent.CLI != "opencode" {
		t.Skipf("AGENT_CLI=%s, skipping opencode default test", cfg.Agent.CLI)
	}
	if cfg.Agent.Model != "deepseek-v4-flash" {
		t.Errorf("expected Agent.Model deepseek-v4-flash, got %s", cfg.Agent.Model)
	}
	if cfg.Agent.ReasoningModel != "deepseek-v4-pro" {
		t.Errorf("expected Agent.ReasoningModel deepseek-v4-pro, got %s", cfg.Agent.ReasoningModel)
	}
}

// TestLoad_ReasoningModelFallsBackToModel covers the claude/codex case: a
// single CLI invocation drives both real task iterations and the
// analyzer/planner fallback (internal/api/server.go's shared `exec`), so if
// only AGENT_MODEL is set, AGENT_REASONING_MODEL should pick it up rather
// than silently defaulting to the CLI's own built-in model.
func TestLoad_ReasoningModelFallsBackToModel(t *testing.T) {
	t.Setenv("AGENT_CLI", "codex")
	t.Setenv("AGENT_MODEL", "gpt-5.5")
	t.Setenv("AGENT_REASONING_MODEL", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.ReasoningModel != "gpt-5.5" {
		t.Errorf("expected Agent.ReasoningModel to fall back to gpt-5.5, got %q", cfg.Agent.ReasoningModel)
	}
}

// TestLoad_ReasoningModelExplicitNotClobbered confirms the fallback only
// fills in an unset AGENT_REASONING_MODEL — an explicit value always wins.
func TestLoad_ReasoningModelExplicitNotClobbered(t *testing.T) {
	t.Setenv("AGENT_CLI", "codex")
	t.Setenv("AGENT_MODEL", "gpt-5.5")
	t.Setenv("AGENT_REASONING_MODEL", "gpt-6")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.ReasoningModel != "gpt-6" {
		t.Errorf("expected explicit Agent.ReasoningModel gpt-6 to be preserved, got %q", cfg.Agent.ReasoningModel)
	}
}

// TestLoad_OpencodeReasoningModelDefaultNotClobbered confirms opencode's own
// paired fast/reasoning model defaults aren't overridden by the
// claude/codex fallback — opencode intentionally uses two different models.
func TestLoad_OpencodeReasoningModelDefaultNotClobbered(t *testing.T) {
	t.Setenv("AGENT_CLI", "opencode")
	t.Setenv("AGENT_MODEL", "custom-fast-model")
	t.Setenv("AGENT_REASONING_MODEL", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.ReasoningModel != "deepseek-v4-pro" {
		t.Errorf("expected opencode's own ReasoningModel default deepseek-v4-pro to survive, got %q", cfg.Agent.ReasoningModel)
	}
}

func TestLoad_StreamMaxCatchUpBacklogDefault(t *testing.T) {
	t.Setenv("STREAM_MAX_CATCHUP_BACKLOG", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Stream.MaxCatchUpBacklog != 100 {
		t.Errorf("expected default Stream.MaxCatchUpBacklog 100, got %d", cfg.Stream.MaxCatchUpBacklog)
	}
}

func TestLoad_StreamMaxCatchUpBacklogOverride(t *testing.T) {
	t.Setenv("STREAM_MAX_CATCHUP_BACKLOG", "50")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Stream.MaxCatchUpBacklog != 50 {
		t.Errorf("expected Stream.MaxCatchUpBacklog 50, got %d", cfg.Stream.MaxCatchUpBacklog)
	}
}

func TestValidate_EmptyRepoCacheRoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RepoCacheRoot = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty RepoCacheRoot")
	}
}

func TestValidate_EmptyLogsRoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogsRoot = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty LogsRoot")
	}
}

func TestValidate_EmptyTmpRoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TmpRoot = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty TmpRoot")
	}
}

func TestValidate_ZeroMaxRuntime(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRuntimeSeconds = 0
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for zero MaxRuntimeSeconds")
	}
}

func TestValidate_NegativeMaxRuntime(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRuntimeSeconds = -1
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for negative MaxRuntimeSeconds")
	}
}

func TestValidate_ZeroMaxConcurrentJobs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrentJobs = 0
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for zero MaxConcurrentJobs")
	}
}

func TestValidate_EmptyBind(t *testing.T) {
	cfg := DefaultConfig()
	cfg.API.Bind = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty API.Bind")
	}
}

func TestIsProjectAllowed_EmptyAllowlist(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowedProjects = []string{}
	if !cfg.IsProjectAllowed("anything") {
		t.Error("empty allowlist should allow all projects")
	}
}

func TestIsProjectAllowed_ExactMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowedProjects = []string{"my-project", "other-project"}

	if !cfg.IsProjectAllowed("my-project") {
		t.Error("expected my-project to be allowed")
	}
	if !cfg.IsProjectAllowed("other-project") {
		t.Error("expected other-project to be allowed")
	}
}

func TestIsProjectAllowed_NoPartialMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowedProjects = []string{"my-project"}

	if cfg.IsProjectAllowed("my-proj") {
		t.Error("partial match should not be allowed")
	}
	if cfg.IsProjectAllowed("my-project-extended") {
		t.Error("extended name should not match")
	}
	if cfg.IsProjectAllowed("other") {
		t.Error("unrelated project should not be allowed")
	}
}

func TestLoadFromEnv_Defaults(t *testing.T) {
	// Clear all env vars that LoadFromEnv reads
	for _, key := range []string{
		"REPO_CACHE_ROOT", "LOGS_ROOT", "TMP_ROOT", "ALLOWED_PROJECTS",
		"JOB_MAX_RUNTIME", "JOB_MAX_CONCURRENT",
		"GIT_PUSH_RETRIES", "GIT_PUSH_RETRY_DELAY_SECONDS",
		"VALIDATION_BLOCK_BINARY_FILES", "VALIDATION_BLOCKED_PATHS",
		"API_BIND", "API_KEY",
		"AGENT_PROMPT_FILE", "AGENT_PATHS",
		"AGENT_AUTHOR", "AGENT_COMMIT_PREFIX",
		"AGENT_MAX_ITERATIONS", "AGENT_TIMEOUT", "AGENT_ITERATION_TIMEOUT",
		"AGENT_MODEL", "AGENT_MAX_TURNS",
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
		"JOB_RETENTION_SECONDS", "CLEANUP_STALE_JOBS",
	} {
		t.Setenv(key, "")
	}
	// Unset them fully so envOrDefault returns defaults
	for _, key := range []string{
		"REPO_CACHE_ROOT", "LOGS_ROOT", "TMP_ROOT", "ALLOWED_PROJECTS",
		"JOB_MAX_RUNTIME", "JOB_MAX_CONCURRENT",
		"GIT_PUSH_RETRIES", "GIT_PUSH_RETRY_DELAY_SECONDS",
		"VALIDATION_BLOCK_BINARY_FILES", "VALIDATION_BLOCKED_PATHS",
		"API_BIND", "API_KEY",
		"AGENT_PROMPT_FILE", "AGENT_PATHS",
		"AGENT_AUTHOR", "AGENT_COMMIT_PREFIX",
		"AGENT_MAX_ITERATIONS", "AGENT_TIMEOUT", "AGENT_ITERATION_TIMEOUT",
		"AGENT_MODEL", "AGENT_MAX_TURNS",
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
		"JOB_RETENTION_SECONDS", "CLEANUP_STALE_JOBS",
	} {
		t.Setenv(key, "")
	}

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxRuntimeSeconds != 300 {
		t.Errorf("expected default MaxRuntimeSeconds 300, got %d", cfg.MaxRuntimeSeconds)
	}
	if cfg.MaxConcurrentJobs != 5 {
		t.Errorf("expected default MaxConcurrentJobs 5, got %d", cfg.MaxConcurrentJobs)
	}
	if cfg.API.Bind != "127.0.0.1:8080" {
		t.Errorf("expected default Bind, got %s", cfg.API.Bind)
	}
}

// TestLoadFromEnv_DataDirFromEnvFile_RelocatesStateDirs guards against a bug
// where DATA_DIR set inside .env (rather than exported as a real OS env var)
// was read too late to affect RepoCacheRoot/LogsRoot/TmpRoot/MemoryDir — those
// silently stayed relative to CWD while only .env.local honored DATA_DIR.
func TestLoadFromEnv_DataDirFromEnvFile_RelocatesStateDirs(t *testing.T) {
	for _, key := range []string{
		"DATA_DIR", "REPO_CACHE_ROOT", "LOGS_ROOT", "TMP_ROOT",
		"OUTPUTS_ROOT", "UPLOADS_ROOT", "MEMORY_DIR", "INSTANCE",
	} {
		t.Setenv(key, "")
	}

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DATA_DIR="+dataDir+"\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Chdir(dir)

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RepoCacheRoot != filepath.Join(dataDir, "repo-cache") {
		t.Errorf("expected RepoCacheRoot under %s, got %s", dataDir, cfg.RepoCacheRoot)
	}
	if cfg.LogsRoot != filepath.Join(dataDir, "logs") {
		t.Errorf("expected LogsRoot under %s, got %s", dataDir, cfg.LogsRoot)
	}
	if cfg.TmpRoot != filepath.Join(dataDir, "tmp") {
		t.Errorf("expected TmpRoot under %s, got %s", dataDir, cfg.TmpRoot)
	}
	if cfg.MemoryDir != filepath.Join(dataDir, "memory") {
		t.Errorf("expected MemoryDir under %s, got %s", dataDir, cfg.MemoryDir)
	}
}

func TestLoadFromEnv_OverridesFromEnv(t *testing.T) {
	t.Setenv("REPO_CACHE_ROOT", "/tmp/repos")
	t.Setenv("LOGS_ROOT", "/tmp/logs")
	t.Setenv("TMP_ROOT", "/tmp/workspaces")
	t.Setenv("JOB_MAX_RUNTIME", "600")
	t.Setenv("JOB_MAX_CONCURRENT", "10")
	t.Setenv("API_BIND", "0.0.0.0:9090")
	t.Setenv("API_KEY", "secret123")
	t.Setenv("ALLOWED_PROJECTS", "project-a,project-b")
	t.Setenv("AGENT_PATHS", "src/,docs/")
	t.Setenv("AGENT_AUTHOR", "my-bot")
	t.Setenv("AGENT_COMMIT_PREFIX", "[auto]")
	t.Setenv("AGENT_MODEL", "qwen3-coder:30b")
	t.Setenv("AGENT_MAX_TURNS", "100")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RepoCacheRoot != "/tmp/repos" {
		t.Errorf("expected /tmp/repos, got %s", cfg.RepoCacheRoot)
	}
	if cfg.MaxRuntimeSeconds != 600 {
		t.Errorf("expected 600, got %d", cfg.MaxRuntimeSeconds)
	}
	if cfg.MaxConcurrentJobs != 10 {
		t.Errorf("expected 10, got %d", cfg.MaxConcurrentJobs)
	}
	if cfg.API.Bind != "0.0.0.0:9090" {
		t.Errorf("expected 0.0.0.0:9090, got %s", cfg.API.Bind)
	}
	if cfg.API.APIKey != "secret123" {
		t.Errorf("expected secret123, got %s", cfg.API.APIKey)
	}
	if len(cfg.AllowedProjects) != 2 {
		t.Errorf("expected 2 allowed projects, got %d", len(cfg.AllowedProjects))
	}
	if len(cfg.Agent.Paths) != 2 {
		t.Errorf("expected 2 agent paths, got %d", len(cfg.Agent.Paths))
	}
	if cfg.Agent.Author != "my-bot" {
		t.Errorf("expected my-bot, got %s", cfg.Agent.Author)
	}
	if cfg.Agent.CommitPrefix != "[auto]" {
		t.Errorf("expected [auto], got %s", cfg.Agent.CommitPrefix)
	}
	if cfg.Agent.Model != "qwen3-coder:30b" {
		t.Errorf("expected qwen3-coder:30b, got %s", cfg.Agent.Model)
	}
	if cfg.Agent.MaxTurns != 100 {
		t.Errorf("expected 100, got %d", cfg.Agent.MaxTurns)
	}
}

func TestLoadFromEnv_InvalidConfig(t *testing.T) {
	t.Setenv("REPO_CACHE_ROOT", "")
	t.Setenv("JOB_MAX_RUNTIME", "0")

	_, err := LoadFromEnv()
	if err == nil {
		t.Error("expected validation error for zero max_runtime_seconds")
	}
}

func TestLoadFromEnv_BoolParsing(t *testing.T) {
	t.Setenv("VALIDATION_BLOCK_BINARY_FILES", "true")
	t.Setenv("CLEANUP_STALE_JOBS", "false")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.Validation.BlockBinaryFiles {
		t.Error("expected BlockBinaryFiles to be true")
	}
	if cfg.StartupCleanupStaleJobs {
		t.Error("expected StartupCleanupStaleJobs to be false")
	}
}

func TestLoad_CombinedProviderModel(t *testing.T) {
	t.Setenv("AGENT_CLI", "opencode")
	t.Setenv("AGENT_MODEL", "myprovider/model-name")
	t.Setenv("AGENT_PROVIDER", "")
	t.Setenv("AGENT_REASONING_MODEL", "openrouter/deepseek/deepseek-r1")
	t.Setenv("AGENT_REASONING_PROVIDER", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.Provider != "myprovider" || cfg.Agent.Model != "model-name" {
		t.Errorf("expected myprovider/model-name split, got %s / %s", cfg.Agent.Provider, cfg.Agent.Model)
	}
	// Multi-slash: first segment is the provider, rest stays as the model.
	if cfg.Agent.ReasoningProvider != "openrouter" || cfg.Agent.ReasoningModel != "deepseek/deepseek-r1" {
		t.Errorf("expected openrouter / deepseek/deepseek-r1, got %s / %s", cfg.Agent.ReasoningProvider, cfg.Agent.ReasoningModel)
	}
}

func TestLoad_ExplicitProviderDisablesSplit(t *testing.T) {
	t.Setenv("AGENT_CLI", "opencode")
	t.Setenv("AGENT_PROVIDER", "openrouter")
	t.Setenv("AGENT_MODEL", "deepseek/deepseek-chat")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.Provider != "openrouter" || cfg.Agent.Model != "deepseek/deepseek-chat" {
		t.Errorf("explicit provider must suppress splitting, got %s / %s", cfg.Agent.Provider, cfg.Agent.Model)
	}
}

func TestFastLLM_DefaultsToAgentFastTier(t *testing.T) {
	t.Setenv("AGENT_CLI", "opencode")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	provider, model, _, _ := cfg.FastLLM()
	if provider != "deepseek" || model != "deepseek-v4-flash" {
		t.Errorf("expected agent fast tier deepseek/deepseek-v4-flash, got %s / %s", provider, model)
	}
}

func TestFastLLM_AnalyzerOverridesAndLegacyPlannerKey(t *testing.T) {
	t.Setenv("AGENT_CLI", "opencode")
	t.Setenv("ANALYZER_MODEL", "anthropic/claude-haiku-4-5-20251001")
	t.Setenv("PLANNER_API_KEY", "legacy-key")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	provider, model, apiKey, _ := cfg.FastLLM()
	if provider != "anthropic" || model != "claude-haiku-4-5-20251001" {
		t.Errorf("expected analyzer pair (combined form) to win, got %s / %s", provider, model)
	}
	if apiKey != "legacy-key" {
		t.Errorf("expected PLANNER_API_KEY fallback, got %q", apiKey)
	}
}

func TestLoadFromEnv_MemoryCurationDefaults(t *testing.T) {
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.MemoryCurationEnabled {
		t.Error("expected curation disabled by default")
	}
	if cfg.Agent.MemoryCurationTimeoutSeconds != 60 {
		t.Errorf("expected default curation timeout 60, got %d", cfg.Agent.MemoryCurationTimeoutSeconds)
	}
}

func TestLoadFromEnv_MemoryCurationOverrides(t *testing.T) {
	t.Setenv("AGENT_MEMORY_CURATION_ENABLED", "true")
	t.Setenv("AGENT_MEMORY_CURATION_TIMEOUT_SECONDS", "120")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Agent.MemoryCurationEnabled {
		t.Error("expected curation enabled")
	}
	if cfg.Agent.MemoryCurationTimeoutSeconds != 120 {
		t.Errorf("expected curation timeout 120, got %d", cfg.Agent.MemoryCurationTimeoutSeconds)
	}
}

func TestLoadFromEnv_TelegramConfig(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:ABC-DEF")
	t.Setenv("TELEGRAM_CHAT_ID", "-1001234567890")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Telegram.BotToken != "123456:ABC-DEF" {
		t.Errorf("expected bot token 123456:ABC-DEF, got %s", cfg.Telegram.BotToken)
	}
	if cfg.Telegram.ChatID != -1001234567890 {
		t.Errorf("expected chat ID -1001234567890, got %d", cfg.Telegram.ChatID)
	}
}

func TestLoadFromEnv_TelegramConfigDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Telegram.BotToken != "" {
		t.Errorf("expected empty bot token, got %s", cfg.Telegram.BotToken)
	}
	if cfg.Telegram.ChatID != 0 {
		t.Errorf("expected chat ID 0, got %d", cfg.Telegram.ChatID)
	}
}

func TestLoadFromEnv_SliceParsing(t *testing.T) {
	t.Setenv("ALLOWED_PROJECTS", "a, b , c")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.AllowedProjects) != 3 {
		t.Fatalf("expected 3 projects, got %d", len(cfg.AllowedProjects))
	}
	if cfg.AllowedProjects[0] != "a" || cfg.AllowedProjects[1] != "b" || cfg.AllowedProjects[2] != "c" {
		t.Errorf("unexpected projects: %v", cfg.AllowedProjects)
	}
}
