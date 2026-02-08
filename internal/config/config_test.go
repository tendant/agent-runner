package config

import (
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ProjectsRoot != "./projects" {
		t.Errorf("expected ProjectsRoot ./projects, got %s", cfg.ProjectsRoot)
	}
	if cfg.RunsRoot != "./runs" {
		t.Errorf("expected RunsRoot ./runs, got %s", cfg.RunsRoot)
	}
	if cfg.TmpRoot != "./tmp" {
		t.Errorf("expected TmpRoot ./tmp, got %s", cfg.TmpRoot)
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
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config should pass validation, got: %v", err)
	}
}

func TestValidate_EmptyProjectsRoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProjectsRoot = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty ProjectsRoot")
	}
}

func TestValidate_EmptyRunsRoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RunsRoot = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty RunsRoot")
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
		"PROJECTS_ROOT", "RUNS_ROOT", "TMP_ROOT", "ALLOWED_PROJECTS",
		"MAX_RUNTIME_SECONDS", "MAX_CONCURRENT_JOBS",
		"GIT_PUSH_RETRIES", "GIT_PUSH_RETRY_DELAY_SECONDS",
		"VALIDATION_BLOCK_BINARY_FILES", "VALIDATION_BLOCKED_PATHS",
		"BIND", "API_KEY",
		"AGENT_PROMPT_FILE", "AGENT_PATHS", "AGENT_DEFAULT_PROJECT",
		"AGENT_AUTHOR", "AGENT_COMMIT_PREFIX",
		"AGENT_MAX_ITERATIONS", "AGENT_MAX_TOTAL_SECONDS", "AGENT_MAX_ITERATION_SECONDS",
		"JOB_RETENTION_SECONDS", "STARTUP_CLEANUP_STALE_JOBS",
	} {
		t.Setenv(key, "")
	}
	// Unset them fully so envOrDefault returns defaults
	for _, key := range []string{
		"PROJECTS_ROOT", "RUNS_ROOT", "TMP_ROOT", "ALLOWED_PROJECTS",
		"MAX_RUNTIME_SECONDS", "MAX_CONCURRENT_JOBS",
		"GIT_PUSH_RETRIES", "GIT_PUSH_RETRY_DELAY_SECONDS",
		"VALIDATION_BLOCK_BINARY_FILES", "VALIDATION_BLOCKED_PATHS",
		"BIND", "API_KEY",
		"AGENT_PROMPT_FILE", "AGENT_PATHS", "AGENT_DEFAULT_PROJECT",
		"AGENT_AUTHOR", "AGENT_COMMIT_PREFIX",
		"AGENT_MAX_ITERATIONS", "AGENT_MAX_TOTAL_SECONDS", "AGENT_MAX_ITERATION_SECONDS",
		"JOB_RETENTION_SECONDS", "STARTUP_CLEANUP_STALE_JOBS",
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

func TestLoadFromEnv_OverridesFromEnv(t *testing.T) {
	t.Setenv("PROJECTS_ROOT", "/tmp/projects")
	t.Setenv("RUNS_ROOT", "/tmp/runs")
	t.Setenv("TMP_ROOT", "/tmp/workspaces")
	t.Setenv("MAX_RUNTIME_SECONDS", "600")
	t.Setenv("MAX_CONCURRENT_JOBS", "10")
	t.Setenv("BIND", "0.0.0.0:9090")
	t.Setenv("API_KEY", "secret123")
	t.Setenv("ALLOWED_PROJECTS", "project-a,project-b")
	t.Setenv("AGENT_DEFAULT_PROJECT", "my-proj")
	t.Setenv("AGENT_PATHS", "src/,docs/")
	t.Setenv("AGENT_AUTHOR", "my-bot")
	t.Setenv("AGENT_COMMIT_PREFIX", "[auto]")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ProjectsRoot != "/tmp/projects" {
		t.Errorf("expected /tmp/projects, got %s", cfg.ProjectsRoot)
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
	if cfg.Agent.DefaultProject != "my-proj" {
		t.Errorf("expected my-proj, got %s", cfg.Agent.DefaultProject)
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
}

func TestLoadFromEnv_InvalidConfig(t *testing.T) {
	t.Setenv("PROJECTS_ROOT", "")
	t.Setenv("MAX_RUNTIME_SECONDS", "0")

	_, err := LoadFromEnv()
	if err == nil {
		t.Error("expected validation error for zero max_runtime_seconds")
	}
}

func TestLoadFromEnv_BoolParsing(t *testing.T) {
	t.Setenv("VALIDATION_BLOCK_BINARY_FILES", "true")
	t.Setenv("STARTUP_CLEANUP_STALE_JOBS", "false")

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
