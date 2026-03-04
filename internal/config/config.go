package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config represents the application configuration
type Config struct {
	// Directory paths
	ProjectDir   string // Resolved CWD at startup — the project root
	ReposRoot    string
	LogsRoot     string
	TmpRoot      string
	TemplatesDir string // Convention: ./templates (user template overrides)
	MemoryDir    string // Convention: ./memory (generated daily logs)

	// Project allowlist
	AllowedProjects []string

	// Execution limits
	MaxRuntimeSeconds int
	MaxConcurrentJobs int

	// Git settings
	GitHost                  string // e.g. "git.memochat.ai"
	GitOrg                   string // e.g. "sites"
	GitPushRetries           int
	GitPushRetryDelaySeconds int

	// Validation settings
	Validation ValidationConfig

	// API settings
	API APIConfig

	// Agent mode settings
	Agent AgentConfig

	// Telegram bot settings
	Telegram TelegramConfig

	// Stream bot settings
	Stream StreamConfig

	// Runner settings
	Runner RunnerConfig

	// Cleanup settings
	JobRetentionSeconds     int
	StartupCleanupStaleJobs bool
}

// AgentConfig contains agent mode settings
type AgentConfig struct {
	MaxIterations       int
	MaxTotalSeconds     int
	MaxIterationSeconds int
	SystemPrompt        string   // Path to base agent prompt (agent.md) — always loaded
	PromptFile          string   // Path to workflow prompt template (prompt.md) — optional overlay
	Paths               []string // Comma-separated allowed paths
	Author              string
	CommitPrefix        string
	Model               string   // Optional: --model flag for Claude CLI (e.g., "qwen3-coder:30b")
	MaxTurns            int      // Optional: --max-turns flag for agentic turns per CLI invocation
	SharedRepos         []string // Repos to pre-populate in every agent workspace (from AGENT_SHARED_REPOS)
	PlannerEnabled      bool     // Enable planner sub-agent before iteration loop
	ReviewerEnabled     bool     // Enable reviewer sub-agent after iteration loop (phase 2)
	MaxQueueSize        int      // Maximum number of queued agent sessions
	MemoryDays          int      // Number of daily memory logs to include (default: 7)
}

// TelegramConfig contains Telegram bot settings
type TelegramConfig struct {
	BotToken string
	ChatID   int64
}

// StreamConfig contains agent-stream bot settings
type StreamConfig struct {
	ServerURL       string   // STREAM_SERVER_URL
	BotToken        string   // STREAM_BOT_TOKEN (pre-registered bot JWT)
	ConversationIDs []string // STREAM_CONVERSATION_IDS
}

// RunnerConfig contains hybrid runner settings
type RunnerConfig struct {
	Enabled           bool   // RUNNER_ENABLED
	DatabaseURL       string // RUNNER_DATABASE_URL (postgres:// or sqlite://)
	AgentID           string // RUNNER_AGENT_ID (default: hostname-pid)
	LeaseDuration     int    // RUNNER_LEASE_DURATION (seconds, default: 60)
	PollCap           int    // RUNNER_POLL_CAP (seconds, default: 30)
	HeartbeatInterval int    // RUNNER_HEARTBEAT_INTERVAL (seconds, default: 300)
	MaxAttempts       int    // RUNNER_MAX_ATTEMPTS (default: 10)
	TypePrefix        string // RUNNER_TYPE_PREFIX (default: "agent.")
}

// ValidationConfig contains diff validation settings
type ValidationConfig struct {
	BlockBinaryFiles bool
	BlockedPaths     []string
}

// APIConfig contains HTTP API settings
type APIConfig struct {
	Bind   string
	APIKey string
}

// DefaultConfig returns a configuration with default values
func DefaultConfig() *Config {
	return &Config{
		ReposRoot:                "./repos",
		LogsRoot:                 "./logs",
		TmpRoot:                  "./tmp",
		TemplatesDir:             "./templates",
		MemoryDir:                "./memory",
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
			Author:              "claude-agent",
			CommitPrefix:        "[agent]",
			MaxTurns:            50,
		PlannerEnabled:      true,
		MaxQueueSize:        10,
		MemoryDays:          7,
		},
		Runner: RunnerConfig{
			LeaseDuration:     60,
			PollCap:           30,
			HeartbeatInterval: 300,
			MaxAttempts:       10,
			TypePrefix:        "agent.",
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: true,
	}
}

// LoadFromEnv loads configuration from environment variables and an optional .env file
func LoadFromEnv() (*Config, error) {
	// Load .env file if present (silently ignore missing)
	_ = godotenv.Load()

	cfg := DefaultConfig()

	// Capture CWD as the project directory — must happen before any chdir
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}
	cfg.ProjectDir = cwd

	cfg.ReposRoot = envOrDefault("REPOS_ROOT", cfg.ReposRoot)
	cfg.LogsRoot = envOrDefault("LOGS_ROOT", cfg.LogsRoot)
	cfg.TmpRoot = envOrDefault("TMP_ROOT", cfg.TmpRoot)
	cfg.AllowedProjects = envSliceOrDefault("ALLOWED_PROJECTS", cfg.AllowedProjects)
	cfg.MaxRuntimeSeconds = envIntOrDefault("MAX_RUNTIME_SECONDS", cfg.MaxRuntimeSeconds)
	cfg.MaxConcurrentJobs = envIntOrDefault("MAX_CONCURRENT_JOBS", cfg.MaxConcurrentJobs)
	cfg.GitHost = envOrDefault("GIT_HOST", cfg.GitHost)
	cfg.GitOrg = envOrDefault("GIT_ORG", cfg.GitOrg)
	cfg.GitPushRetries = envIntOrDefault("GIT_PUSH_RETRIES", cfg.GitPushRetries)
	cfg.GitPushRetryDelaySeconds = envIntOrDefault("GIT_PUSH_RETRY_DELAY_SECONDS", cfg.GitPushRetryDelaySeconds)

	cfg.Validation.BlockBinaryFiles = envBoolOrDefault("VALIDATION_BLOCK_BINARY_FILES", cfg.Validation.BlockBinaryFiles)
	cfg.Validation.BlockedPaths = envSliceOrDefault("VALIDATION_BLOCKED_PATHS", cfg.Validation.BlockedPaths)

	cfg.API.Bind = envOrDefault("BIND", cfg.API.Bind)
	cfg.API.APIKey = envOrDefault("API_KEY", cfg.API.APIKey)

	cfg.Agent.SystemPrompt = envOrDefault("AGENT_SYSTEM_PROMPT", cfg.Agent.SystemPrompt)
	cfg.Agent.PromptFile = envOrDefault("AGENT_PROMPT_FILE", cfg.Agent.PromptFile)
	cfg.Agent.Paths = envSliceOrDefault("AGENT_PATHS", cfg.Agent.Paths)
	cfg.Agent.Author = envOrDefault("AGENT_AUTHOR", cfg.Agent.Author)
	cfg.Agent.CommitPrefix = envOrDefault("AGENT_COMMIT_PREFIX", cfg.Agent.CommitPrefix)
	cfg.Agent.MaxIterations = envIntOrDefault("AGENT_MAX_ITERATIONS", cfg.Agent.MaxIterations)
	cfg.Agent.MaxTotalSeconds = envIntOrDefault("AGENT_MAX_TOTAL_SECONDS", cfg.Agent.MaxTotalSeconds)
	cfg.Agent.MaxIterationSeconds = envIntOrDefault("AGENT_MAX_ITERATION_SECONDS", cfg.Agent.MaxIterationSeconds)
	cfg.Agent.Model = envOrDefault("AGENT_MODEL", cfg.Agent.Model)
	cfg.Agent.MaxTurns = envIntOrDefault("AGENT_MAX_TURNS", cfg.Agent.MaxTurns)
	cfg.Agent.SharedRepos = envSliceOrDefault("AGENT_SHARED_REPOS", cfg.Agent.SharedRepos)
	cfg.Agent.PlannerEnabled = envBoolOrDefault("AGENT_PLANNER_ENABLED", cfg.Agent.PlannerEnabled)
	cfg.Agent.ReviewerEnabled = envBoolOrDefault("AGENT_REVIEWER_ENABLED", cfg.Agent.ReviewerEnabled)
	cfg.Agent.MaxQueueSize = envIntOrDefault("AGENT_MAX_QUEUE_SIZE", cfg.Agent.MaxQueueSize)
	cfg.Agent.MemoryDays = envIntOrDefault("AGENT_MEMORY_DAYS", cfg.Agent.MemoryDays)

	cfg.Telegram.BotToken = envOrDefault("TELEGRAM_BOT_TOKEN", "")
	cfg.Telegram.ChatID = envInt64OrDefault("TELEGRAM_CHAT_ID", 0)

	cfg.Stream.ServerURL = envOrDefault("STREAM_SERVER_URL", "")
	cfg.Stream.BotToken = envOrDefault("STREAM_BOT_TOKEN", "")
	cfg.Stream.ConversationIDs = envSliceOrDefault("STREAM_CONVERSATION_IDS", nil)

	cfg.Runner.Enabled = envBoolOrDefault("RUNNER_ENABLED", cfg.Runner.Enabled)
	cfg.Runner.DatabaseURL = envOrDefault("RUNNER_DATABASE_URL", cfg.Runner.DatabaseURL)
	cfg.Runner.AgentID = envOrDefault("RUNNER_AGENT_ID", cfg.Runner.AgentID)
	cfg.Runner.LeaseDuration = envIntOrDefault("RUNNER_LEASE_DURATION", cfg.Runner.LeaseDuration)
	cfg.Runner.PollCap = envIntOrDefault("RUNNER_POLL_CAP", cfg.Runner.PollCap)
	cfg.Runner.HeartbeatInterval = envIntOrDefault("RUNNER_HEARTBEAT_INTERVAL", cfg.Runner.HeartbeatInterval)
	cfg.Runner.MaxAttempts = envIntOrDefault("RUNNER_MAX_ATTEMPTS", cfg.Runner.MaxAttempts)
	cfg.Runner.TypePrefix = envOrDefault("RUNNER_TYPE_PREFIX", cfg.Runner.TypePrefix)

	cfg.JobRetentionSeconds = envIntOrDefault("JOB_RETENTION_SECONDS", cfg.JobRetentionSeconds)
	cfg.StartupCleanupStaleJobs = envBoolOrDefault("STARTUP_CLEANUP_STALE_JOBS", cfg.StartupCleanupStaleJobs)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.ReposRoot == "" {
		return fmt.Errorf("repos_root is required")
	}
	if c.LogsRoot == "" {
		return fmt.Errorf("logs_root is required")
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

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envInt64OrDefault(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envBoolOrDefault(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envSliceOrDefault(key string, fallback []string) []string {
	if v := os.Getenv(key); v != "" {
		parts := strings.Split(v, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		return result
	}
	return fallback
}
