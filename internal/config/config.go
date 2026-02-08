package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the application configuration
type Config struct {
	// Directory paths
	ProjectsRoot string `yaml:"projects_root"`
	RunsRoot     string `yaml:"runs_root"`
	TmpRoot      string `yaml:"tmp_root"`

	// Project allowlist
	AllowedProjects []string `yaml:"allowed_projects"`

	// Execution limits
	MaxRuntimeSeconds  int `yaml:"max_runtime_seconds"`
	MaxConcurrentJobs  int `yaml:"max_concurrent_jobs"`

	// Git settings
	GitPushRetries           int `yaml:"git_push_retries"`
	GitPushRetryDelaySeconds int `yaml:"git_push_retry_delay_seconds"`

	// Validation settings
	Validation ValidationConfig `yaml:"validation"`

	// API settings
	API APIConfig `yaml:"api"`

	// Agent mode settings
	Agent AgentConfig `yaml:"agent"`

	// Cleanup settings
	JobRetentionSeconds      int  `yaml:"job_retention_seconds"`
	StartupCleanupStaleJobs  bool `yaml:"startup_cleanup_stale_jobs"`
}

// AgentConfig contains agent mode settings
type AgentConfig struct {
	MaxIterations       int `yaml:"max_iterations"`
	MaxTotalSeconds     int `yaml:"max_total_seconds"`
	MaxIterationSeconds int `yaml:"max_iteration_seconds"`
}

// ValidationConfig contains diff validation settings
type ValidationConfig struct {
	BlockBinaryFiles bool     `yaml:"block_binary_files"`
	BlockedPaths     []string `yaml:"blocked_paths"`
}

// APIConfig contains HTTP API settings
type APIConfig struct {
	Bind   string `yaml:"bind"`
	APIKey string `yaml:"api_key"`
}

// DefaultConfig returns a configuration with default values
func DefaultConfig() *Config {
	return &Config{
		ProjectsRoot:             "./projects",
		RunsRoot:                 "./runs",
		TmpRoot:                  "./tmp",
		AllowedProjects:          []string{},
		MaxRuntimeSeconds:        300,
		MaxConcurrentJobs:        5,
		GitPushRetries:           3,
		GitPushRetryDelaySeconds: 5,
		Validation: ValidationConfig{
			BlockBinaryFiles: false,
			BlockedPaths: []string{
				".git/",
				".github/",
				".gitlab-ci.yml",
				"secrets/",
				"*.env",
			},
		},
		API: APIConfig{
			Bind:   "127.0.0.1:8080",
			APIKey: "",
		},
		Agent: AgentConfig{
			MaxIterations:       50,
			MaxTotalSeconds:     3600,
			MaxIterationSeconds: 300,
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: true,
	}
}

// Load reads configuration from a YAML file
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.ProjectsRoot == "" {
		return fmt.Errorf("projects_root is required")
	}
	if c.RunsRoot == "" {
		return fmt.Errorf("runs_root is required")
	}
	if c.TmpRoot == "" {
		return fmt.Errorf("tmp_root is required")
	}
	if c.MaxRuntimeSeconds <= 0 {
		return fmt.Errorf("max_runtime_seconds must be positive")
	}
	if c.MaxConcurrentJobs <= 0 {
		return fmt.Errorf("max_concurrent_jobs must be positive")
	}
	if c.API.Bind == "" {
		return fmt.Errorf("api.bind is required")
	}
	return nil
}

// IsProjectAllowed checks if a project is in the allowlist
func (c *Config) IsProjectAllowed(project string) bool {
	if len(c.AllowedProjects) == 0 {
		return true // No allowlist means all projects are allowed
	}
	for _, p := range c.AllowedProjects {
		if p == project {
			return true
		}
	}
	return false
}
