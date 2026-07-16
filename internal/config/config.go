package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// defaultDataDir returns the default base directory for all mutable agent-runner
// data: the process working directory. Using CWD means each agent-runner instance
// running from its own directory gets isolated data without any explicit DATA_DIR.
func defaultDataDir() string {
	return "."
}

// Config represents the application configuration
type Config struct {
	// Directory paths
	ProjectDir   string // Resolved CWD at startup — the project root
	RepoCacheRoot string
	LogsRoot      string
	TmpRoot       string
	OutputsRoot   string // persistent storage for _send/ files across sessions
	UploadsRoot   string // persistent storage for files uploaded by users via bots
	MemoryDir string // Convention: ./memory (seeded defaults + daily logs + curated memory)

	// Project allowlist
	AllowedProjects []string

	// Execution limits
	MaxRuntimeSeconds int
	MaxConcurrentJobs int

	// Git settings
	GitHost                  string // e.g. "gitea.example.com"
	GitOrg                   string // e.g. "sites"
	GitToken                 string // GIT_TOKEN — injected into HTTPS repo URLs at runtime
	GitSSHKey                string // GIT_SSH_KEY — path to SSH private key for repo operations
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

	// WeChat bot settings
	WeChat WeChatConfig

	// Analyzer LLM settings
	Analyzer AnalyzerConfig

	// Runner settings
	Runner RunnerConfig

	// Cleanup settings
	JobRetentionSeconds     int
	StartupCleanupStaleJobs bool
	LogRetentionDays        int // Delete log files older than this many days (0 = keep forever)
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
	Provider            string   // Optional: model provider (e.g., "deepseek", "openrouter") — opencode only; prepended to Model as "provider/model"
	Model               string   // Optional: model name (e.g., "deepseek-chat"); combined with Provider when set
	ReasoningProvider   string   // Optional: provider for agent CLI execution (e.g., "openai"); omit to use CLI default
	ReasoningModel      string   // Optional: model for agent CLI execution; omit to use CLI default
	MaxTurns            int      // Optional: --max-turns flag for agentic turns per CLI invocation
	CLI                 string   // CLI backend: "claude" (default), "codex", or "opencode"
	SharedRepos         []string // Repos to pre-populate in every agent workspace (from AGENT_SHARED_REPOS)
	SkillsDir           string   // AGENT_SKILLS_DIR — directory of skills pre-populated in every workspace
	APIKey              string   // Optional: API key for the planner LLM (PLANNER_API_KEY)
	BaseURL             string   // Optional: base URL override for the planner LLM (PLANNER_BASE_URL)
	PlannerEnabled      bool     // Enable planner sub-agent before iteration loop
	ReviewerEnabled     bool     // Enable reviewer sub-agent after iteration loop (phase 2)
	MaxQueueSize        int      // Maximum number of queued agent sessions
	MemoryDays          int      // Number of daily memory logs to include (default: 7)
	MemoryPullOnStart   bool     // Pull memory from git before each session
	MemoryCharCap       int      // Max characters in composed memory section (0 = no limit)
}

// TelegramConfig contains Telegram bot settings
type TelegramConfig struct {
	BotToken string
	ChatID   int64
}

// StreamConfig contains agent-stream bot settings
type StreamConfig struct {
	ServerURL       string        // STREAM_SERVER_URL
	BotToken        string        // STREAM_BOT_TOKEN (pre-registered bot JWT)
	ConversationIDs []string      // STREAM_CONVERSATION_IDS
	PollInterval    time.Duration // STREAM_POLL_INTERVAL — if set, use polling instead of SSE (e.g. "3s")
	StateDir        string        // STREAM_STATE_DIR — directory for the per-conversation event-seq cursor; defaults to TmpRoot
}

// WeChatConfig contains WeChat bot settings (iLink API).
type WeChatConfig struct {
	Token      string // WECHAT_TOKEN — bearer token obtained via QR login
	BaseURL    string // WECHAT_BASE_URL — default: https://ilinkai.weixin.qq.com
	CDNBaseURL string // WECHAT_CDN_BASE_URL — media CDN; default: https://novac2c.cdn.weixin.qq.com/c2c
	StateDir   string // WECHAT_STATE_DIR — directory for sync buf cursor; defaults to TmpRoot
}

// AnalyzerConfig configures the fast LLM used for conversation intent routing.
// If no provider/key is set, falls back to the executor (Claude CLI).
type AnalyzerConfig struct {
	Provider       string // "anthropic" | "openai" | "" (auto-detect from env)
	Model          string // model ID; provider default used if empty
	APIKey         string // ANALYZER_API_KEY; falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY
	BaseURL        string // override API base URL (e.g. http://localhost:11434 for Ollama)
	TimeoutSeconds int    // per-call timeout; default 30s
}

// RunnerConfig contains hybrid runner settings
type RunnerConfig struct {
	Enabled           bool   // SCHEDULER_ENABLED
	DatabaseURL       string // SCHEDULER_DATABASE_URL (postgres:// or sqlite://)
	AgentID           string // SCHEDULER_AGENT_ID (default: hostname-pid)
	LeaseDuration     int    // SCHEDULER_LEASE_DURATION (seconds, default: 60)
	PollCap           int    // SCHEDULER_POLL_CAP (seconds, default: 30)
	HeartbeatInterval int    // SCHEDULER_HEARTBEAT_INTERVAL (seconds, default: 300)
	MaxAttempts       int    // SCHEDULER_MAX_ATTEMPTS (default: 10)
	TypePrefix        string // SCHEDULER_TYPE_PREFIX (default: "agent.")
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
	data := defaultDataDir()
	return &Config{
		RepoCacheRoot:           filepath.Join(data, "repo-cache"),
		LogsRoot:                filepath.Join(data, "logs"),
		TmpRoot:                 filepath.Join(data, "tmp"),
		OutputsRoot:             filepath.Join(data, "outputs"),
		UploadsRoot:             filepath.Join(data, "uploads"),
		MemoryDir:               filepath.Join(data, "memory"),
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
			MaxIterations:       20,
			MaxTotalSeconds:     3600,
			MaxIterationSeconds: 300,
			Author:              "claude-agent",
			CommitPrefix:        "[agent]",
			MaxTurns:            50,
			CLI:                 "opencode",
			PlannerEnabled:      true,
			MaxQueueSize:        10,
			MemoryDays:          7,
			MemoryCharCap:       12000,
			MemoryPullOnStart:   true,
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
		LogRetentionDays:        30,
	}
}

// LoadFromEnv loads configuration from environment variables and optional env
// files. Priority order (highest to lowest):
//
//  1. OS environment      — always wins
//  2. DATA_DIR/.env.local — written by /set; survives image updates
//  3. .env.<instance>     — instance-specific overrides (when INSTANCE is set)
//  4. .env                — base config; committed as a template
//
// When INSTANCE is set (e.g. INSTANCE=prod), the app loads .env.prod and
// defaults DATA_DIR to the process working directory, isolating each instance's data.
func LoadFromEnv() (*Config, error) {
	// Determine instance name. Drives the default data dir and instance env file.
	instance := os.Getenv("INSTANCE")

	// Determine data dir: explicit DATA_DIR wins, then instance-scoped default,
	// then global default.
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		base := defaultDataDir()
		if instance != "" {
			dataDir = filepath.Join(base, instance)
		} else {
			dataDir = base
		}
	}

	// Wire SetEnvLocal to write to the data dir's .env.local.
	envLocalPath = filepath.Join(dataDir, ".env.local")

	// Read files lowest-priority first; each layer overwrites keys from the one below.
	// Priority: .env < .env.<instance> < DATA_DIR/.env.local < OS env
	merged, _ := godotenv.Read(".env")
	if instance != "" {
		inst, _ := godotenv.Read(".env." + instance)
		for k, v := range inst {
			merged[k] = v
		}
	}
	local, _ := godotenv.Read(envLocalPath)
	for k, v := range local {
		merged[k] = v
	}
	for k, v := range merged {
		if os.Getenv(k) == "" {
			if err := os.Setenv(k, v); err != nil {
				slog.Warn("config: failed to set env var from .env", "key", k, "error", err)
			}
		}
	}

	cfg := DefaultConfig()

	// Capture CWD as the project directory — must happen before any chdir
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}
	cfg.ProjectDir = cwd

	cfg.RepoCacheRoot = envOrDefault("REPO_CACHE_ROOT", cfg.RepoCacheRoot)
	cfg.LogsRoot = envOrDefault("LOGS_ROOT", cfg.LogsRoot)
	cfg.TmpRoot = envOrDefault("TMP_ROOT", cfg.TmpRoot)
	cfg.OutputsRoot = envOrDefault("OUTPUTS_ROOT", cfg.OutputsRoot)
	cfg.UploadsRoot = envOrDefault("UPLOADS_ROOT", cfg.UploadsRoot)
	cfg.MemoryDir = envOrDefault("MEMORY_DIR", cfg.MemoryDir)
	cfg.AllowedProjects = envSliceOrDefault("ALLOWED_PROJECTS", cfg.AllowedProjects)
	cfg.MaxRuntimeSeconds = envIntOrDefault("JOB_MAX_RUNTIME", cfg.MaxRuntimeSeconds)
	cfg.MaxConcurrentJobs = envIntOrDefault("JOB_MAX_CONCURRENT", cfg.MaxConcurrentJobs)
	cfg.GitHost = envOrDefault("GIT_HOST", cfg.GitHost)
	cfg.GitOrg = envOrDefault("GIT_ORG", cfg.GitOrg)
	cfg.GitToken = envOrDefault("GIT_TOKEN", cfg.GitToken)
	cfg.GitSSHKey = envOrDefault("GIT_SSH_KEY", cfg.GitSSHKey)
	cfg.GitPushRetries = envIntOrDefault("GIT_PUSH_RETRIES", cfg.GitPushRetries)
	cfg.GitPushRetryDelaySeconds = envIntOrDefault("GIT_PUSH_RETRY_DELAY_SECONDS", cfg.GitPushRetryDelaySeconds)

	// Propagate GIT_SSH_KEY into GIT_SSH_COMMAND so all git subprocesses
	// (including the agent CLI) inherit it without extra wiring.
	if cfg.GitSSHKey != "" && os.Getenv("GIT_SSH_COMMAND") == "" {
		if err := os.Setenv("GIT_SSH_COMMAND", "ssh -i "+cfg.GitSSHKey+" -o StrictHostKeyChecking=no"); err != nil {
			slog.Warn("config: failed to set GIT_SSH_COMMAND", "error", err)
		}
	}

	cfg.Validation.BlockBinaryFiles = envBoolOrDefault("VALIDATION_BLOCK_BINARY_FILES", cfg.Validation.BlockBinaryFiles)
	cfg.Validation.BlockedPaths = envSliceOrDefault("VALIDATION_BLOCKED_PATHS", cfg.Validation.BlockedPaths)

	cfg.API.Bind = envOrDefault("API_BIND", cfg.API.Bind)
	cfg.API.APIKey = envOrDefault("API_KEY", cfg.API.APIKey)

	cfg.Agent.SystemPrompt = envOrDefault("AGENT_SYSTEM_PROMPT", cfg.Agent.SystemPrompt)
	cfg.Agent.PromptFile = envOrDefault("AGENT_PROMPT_FILE", cfg.Agent.PromptFile)
	cfg.Agent.Paths = envSliceOrDefault("AGENT_PATHS", cfg.Agent.Paths)
	cfg.Agent.Author = envOrDefault("AGENT_AUTHOR", cfg.Agent.Author)
	cfg.Agent.CommitPrefix = envOrDefault("AGENT_COMMIT_PREFIX", cfg.Agent.CommitPrefix)
	cfg.Agent.MaxIterations = envIntOrDefault("AGENT_MAX_ITERATIONS", cfg.Agent.MaxIterations)
	cfg.Agent.MaxTotalSeconds = envIntOrDefault("AGENT_MAX_TOTAL_SECONDS", cfg.Agent.MaxTotalSeconds)
	cfg.Agent.MaxIterationSeconds = envIntOrDefault("AGENT_MAX_ITERATION_SECONDS", cfg.Agent.MaxIterationSeconds)
	cfg.Agent.CLI = envOrDefault("AGENT_CLI", cfg.Agent.CLI)
	// opencode requires explicit model/provider; set defaults unless overridden below.
	if cfg.Agent.CLI == "opencode" || cfg.Agent.CLI == "" {
		cfg.Agent.Provider = "deepseek"
		cfg.Agent.Model = "deepseek-v4-flash"
		cfg.Agent.ReasoningProvider = "deepseek"
		cfg.Agent.ReasoningModel = "deepseek-v4-pro"
	}
	cfg.Agent.Provider = envOrDefault("AGENT_PROVIDER", cfg.Agent.Provider)
	cfg.Agent.Model = envOrDefault("AGENT_MODEL", cfg.Agent.Model)
	cfg.Agent.ReasoningProvider = envOrDefault("AGENT_REASONING_PROVIDER", cfg.Agent.ReasoningProvider)
	cfg.Agent.ReasoningModel = envOrDefault("AGENT_REASONING_MODEL", cfg.Agent.ReasoningModel)
	cfg.Agent.MaxTurns = envIntOrDefault("AGENT_MAX_TURNS", cfg.Agent.MaxTurns)
	cfg.Agent.SharedRepos = envSliceOrDefault("AGENT_SHARED_REPOS", cfg.Agent.SharedRepos)
	cfg.Agent.SkillsDir = os.Getenv("AGENT_SKILLS_DIR")
	cfg.Agent.APIKey = envOrDefault("PLANNER_API_KEY", cfg.Agent.APIKey)
	cfg.Agent.BaseURL = envOrDefault("PLANNER_BASE_URL", cfg.Agent.BaseURL)
	cfg.Agent.PlannerEnabled = envBoolOrDefault("AGENT_PLANNER_ENABLED", cfg.Agent.PlannerEnabled)
	cfg.Agent.ReviewerEnabled = envBoolOrDefault("AGENT_REVIEWER_ENABLED", cfg.Agent.ReviewerEnabled)
	cfg.Agent.MaxQueueSize = envIntOrDefault("AGENT_MAX_QUEUE_SIZE", cfg.Agent.MaxQueueSize)
	cfg.Agent.MemoryDays = envIntOrDefault("AGENT_MEMORY_DAYS", cfg.Agent.MemoryDays)
	cfg.Agent.MemoryPullOnStart = envBoolOrDefault("AGENT_MEMORY_PULL_ON_START", cfg.Agent.MemoryPullOnStart)
	cfg.Agent.MemoryCharCap = envIntOrDefault("AGENT_MEMORY_CHAR_CAP", cfg.Agent.MemoryCharCap)

	cfg.Telegram.BotToken = envOrDefault("TELEGRAM_BOT_TOKEN", "")
	cfg.Telegram.ChatID = envInt64OrDefault("TELEGRAM_CHAT_ID", 0)

	cfg.Stream.ServerURL = envOrDefault("STREAM_SERVER_URL", "")
	cfg.Stream.BotToken = envOrDefault("STREAM_BOT_TOKEN", "")
	cfg.Stream.ConversationIDs = envSliceOrDefault("STREAM_CONVERSATION_IDS", nil)
	if v := os.Getenv("STREAM_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Stream.PollInterval = d
		}
	}
	cfg.Stream.StateDir = envOrDefault("STREAM_STATE_DIR", cfg.TmpRoot)

	cfg.WeChat.Token = envOrDefault("WECHAT_TOKEN", "")
	cfg.WeChat.BaseURL = envOrDefault("WECHAT_BASE_URL", "https://ilinkai.weixin.qq.com")
	cfg.WeChat.CDNBaseURL = envOrDefault("WECHAT_CDN_BASE_URL", "")
	cfg.WeChat.StateDir = envOrDefault("WECHAT_STATE_DIR", cfg.TmpRoot)

	cfg.Analyzer.Provider = envOrDefault("ANALYZER_PROVIDER", "")
	cfg.Analyzer.Model = envOrDefault("ANALYZER_MODEL", "")
	cfg.Analyzer.APIKey = envOrDefault("ANALYZER_API_KEY", "")
	cfg.Analyzer.BaseURL = envOrDefault("ANALYZER_BASE_URL", "")
	cfg.Analyzer.TimeoutSeconds = envIntOrDefault("ANALYZER_TIMEOUT_SECONDS", 30)

	cfg.Runner.Enabled = envBoolOrDefault("SCHEDULER_ENABLED", cfg.Runner.Enabled)
	cfg.Runner.DatabaseURL = envOrDefault("SCHEDULER_DATABASE_URL", cfg.Runner.DatabaseURL)
	cfg.Runner.AgentID = envOrDefault("SCHEDULER_AGENT_ID", cfg.Runner.AgentID)
	cfg.Runner.LeaseDuration = envIntOrDefault("SCHEDULER_LEASE_DURATION", cfg.Runner.LeaseDuration)
	cfg.Runner.PollCap = envIntOrDefault("SCHEDULER_POLL_CAP", cfg.Runner.PollCap)
	cfg.Runner.HeartbeatInterval = envIntOrDefault("SCHEDULER_HEARTBEAT_INTERVAL", cfg.Runner.HeartbeatInterval)
	cfg.Runner.MaxAttempts = envIntOrDefault("SCHEDULER_MAX_ATTEMPTS", cfg.Runner.MaxAttempts)
	cfg.Runner.TypePrefix = envOrDefault("SCHEDULER_TYPE_PREFIX", cfg.Runner.TypePrefix)

	cfg.JobRetentionSeconds = envIntOrDefault("JOB_RETENTION_SECONDS", cfg.JobRetentionSeconds)
	cfg.StartupCleanupStaleJobs = envBoolOrDefault("CLEANUP_STALE_JOBS", cfg.StartupCleanupStaleJobs)
	cfg.LogRetentionDays = envIntOrDefault("LOG_RETENTION_DAYS", cfg.LogRetentionDays)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.RepoCacheRoot == "" {
		return fmt.Errorf("repo_cache_root is required")
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
