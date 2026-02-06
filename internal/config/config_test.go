package config

import (
	"os"
	"path/filepath"
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

func TestLoad_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	content := `
projects_root: /tmp/projects
runs_root: /tmp/runs
tmp_root: /tmp/workspaces
max_runtime_seconds: 600
max_concurrent_jobs: 10
api:
  bind: "0.0.0.0:9090"
  api_key: "secret123"
allowed_projects:
  - project-a
  - project-b
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(cfgPath)
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
}

func TestLoad_OverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Only override some fields — rest should keep defaults
	content := `
max_runtime_seconds: 120
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxRuntimeSeconds != 120 {
		t.Errorf("expected 120, got %d", cfg.MaxRuntimeSeconds)
	}
	// Defaults should be preserved
	if cfg.ProjectsRoot != "./projects" {
		t.Errorf("expected default ProjectsRoot, got %s", cfg.ProjectsRoot)
	}
	if cfg.MaxConcurrentJobs != 5 {
		t.Errorf("expected default MaxConcurrentJobs 5, got %d", cfg.MaxConcurrentJobs)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(cfgPath, []byte("{{{{invalid yaml"), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoad_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	content := `
projects_root: ""
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Error("expected validation error for empty projects_root")
	}
}
