package main

import (
	"context"
	"log"
	"log/slog"
	"time"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/runner"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
	simpleworkflow "github.com/tendant/simple-workflow"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Log configuration summary
	slog.Info("configuration loaded",
		"project_dir", cfg.ProjectDir,
		"repos_root", cfg.ReposRoot,
		"logs_root", cfg.LogsRoot,
		"tmp_root", cfg.TmpRoot,
		"memory_dir", cfg.MemoryDir,
		"max_runtime_seconds", cfg.MaxRuntimeSeconds,
		"max_concurrent_jobs", cfg.MaxConcurrentJobs,
	)
	if len(cfg.AllowedProjects) > 0 {
		slog.Info("allowed projects", "projects", cfg.AllowedProjects)
	} else {
		slog.Info("allowed projects: all")
	}
	if cfg.API.APIKey != "" {
		slog.Info("API key authentication enabled")
	}
	if cfg.Agent.Model != "" {
		slog.Info("agent configured", "model", cfg.Agent.Model)
	}
	if cfg.Agent.PromptFile != "" {
		slog.Info("agent prompt file", "path", cfg.Agent.PromptFile)
	}
	if cfg.Telegram.BotToken != "" {
		slog.Info("telegram bot enabled")
	}

	// Seed embedded defaults into memory dir on first run
	if err := tmpl.SeedDefaults(cfg.MemoryDir); err != nil {
		slog.Warn("failed to seed defaults", "error", err)
	}
	// Refresh unmodified defaults to pick up new embedded versions
	if err := tmpl.RefreshDefaults(cfg.MemoryDir); err != nil {
		slog.Warn("failed to refresh defaults", "error", err)
	}

	// Prompt files (AGENT_SYSTEM_PROMPT, AGENT_PROMPT_FILE) are now loaded
	// directly from source on each session via LoadPromptFile — no need to
	// seed copies into the memory directory.

	// Create and start server
	server := api.NewServer(cfg)

	// Conditionally start hybrid runner
	if cfg.Runner.Enabled {
		slog.Info("runner enabled",
			"db", cfg.Runner.DatabaseURL, "prefix", cfg.Runner.TypePrefix,
			"lease_seconds", cfg.Runner.LeaseDuration, "poll_cap_seconds", cfg.Runner.PollCap)
		if cfg.Runner.DatabaseURL == "" {
			log.Fatalf("RUNNER_DATABASE_URL is required when RUNNER_SCHEDULER_ENABLED=true")
		}

		bridge := api.NewRunnerBridge(server.Handlers())
		r, err := runner.New(runner.Config{
			DatabaseURL:       cfg.Runner.DatabaseURL,
			AgentID:           cfg.Runner.AgentID,
			LeaseDuration:     time.Duration(cfg.Runner.LeaseDuration) * time.Second,
			PollCap:           time.Duration(cfg.Runner.PollCap) * time.Second,
			HeartbeatInterval: time.Duration(cfg.Runner.HeartbeatInterval) * time.Second,
			MaxAttempts:       cfg.Runner.MaxAttempts,
			TypePrefix:        cfg.Runner.TypePrefix,
			MemoryDir:         cfg.MemoryDir,
		}, bridge)
		if err != nil {
			log.Fatalf("Failed to create runner: %v", err)
		}
		server.SetRunner(r)

		// Create workflow client for agent scheduling
		swClient := simpleworkflow.NewClientWithDB(r.DB(), r.Dialect())
		server.Handlers().SetWorkflowClient(api.NewWorkflowScheduler(swClient))
		server.Handlers().SetRunnerDB(runner.NewDebugDB(r.DB(), r.Dialect().DriverName()))
		slog.Info("runner workflow client initialized")

		go func() {
			slog.Info("runner starting")
			if err := r.Start(context.Background()); err != nil {
				slog.Error("runner error", "error", err)
			}
		}()
	} else {
		slog.Info("runner disabled (set RUNNER_SCHEDULER_ENABLED=true and RUNNER_DATABASE_URL to enable)")
	}

	if err := server.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
