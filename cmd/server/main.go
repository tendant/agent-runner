package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/scheduler"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
	simpleworkflow "github.com/tendant/simple-workflow"
)

// Build-time variables — set via -ldflags "-X main.buildTime=..."
var buildTime = "unknown"

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.Info("agent-runner starting", "built", buildTime)

	// Log configuration summary
	slog.Info("configuration loaded",
		"project_dir", cfg.ProjectDir,
		"repo_cache_root", cfg.RepoCacheRoot,
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
	if cfg.Agent.Model != "" || cfg.Agent.FastModel != "" {
		// reasoning_model is what actually reaches the CLI invocation for both
		// real task iterations and the analyzer/planner fallback — logged
		// explicitly since it silently defaulted to the CLI's own built-in
		// model (not "model") whenever it wasn't set.
		slog.Info("agent configured", "cli", cfg.Agent.CLI, "model", cfg.Agent.Model, "fast_model", cfg.Agent.FastModel)
	}
	for _, w := range api.BootstrapWarnings(cfg.Agent.CLI, cfg.Agent.Provider) {
		slog.Warn("startup warning", "msg", w)
	}
	if cfg.Agent.PromptFile != "" {
		slog.Info("agent prompt file", "path", cfg.Agent.PromptFile)
	}
	if cfg.Telegram.BotToken != "" {
		slog.Info("telegram bot enabled")
	}

	// Log memory contents at startup. Sessions pull from git themselves
	// (MemoryPullOnStart=true by default), so no pull here to avoid double-pulling
	// on the first session after reboot.
	if cfg.MemoryDir != "" {
		retrieval := tmpl.Retrieve(cfg.MemoryDir)
		if len(retrieval.Files) > 0 {
			names := make([]string, len(retrieval.Files))
			for i, f := range retrieval.Files {
				names[i] = f.Name
			}
			slog.Info("memory loaded", "files", len(retrieval.Files), "sections", strings.Join(names, ", "))
		} else {
			slog.Info("memory loaded", "files", 0)
		}
	}

	// Create and start server
	server := api.NewServer(cfg)

	// Conditionally start hybrid scheduler
	if cfg.Scheduler.Enabled {
		slog.Info("scheduler enabled",
			"db", cfg.Scheduler.DatabaseURL, "prefix", cfg.Scheduler.TypePrefix,
			"lease_seconds", cfg.Scheduler.LeaseDuration, "poll_cap_seconds", cfg.Scheduler.PollCap)
		if cfg.Scheduler.DatabaseURL == "" {
			slog.Error("SCHEDULER_DATABASE_URL is required when SCHEDULER_ENABLED=true")
			os.Exit(1)
		}

		bridge := api.NewRunnerBridge(server.Handlers())
		r, err := scheduler.New(scheduler.Config{
			DatabaseURL:       cfg.Scheduler.DatabaseURL,
			AgentID:           cfg.Scheduler.AgentID,
			LeaseDuration:     time.Duration(cfg.Scheduler.LeaseDuration) * time.Second,
			PollCap:           time.Duration(cfg.Scheduler.PollCap) * time.Second,
			HeartbeatInterval: time.Duration(cfg.Scheduler.HeartbeatInterval) * time.Second,
			MaxAttempts:       cfg.Scheduler.MaxAttempts,
			TypePrefix:        cfg.Scheduler.TypePrefix,
			MemoryDir:         cfg.MemoryDir,
		}, bridge)
		if err != nil {
			slog.Error("failed to create scheduler", "error", err)
			os.Exit(1)
		}
		server.SetScheduler(r)

		// Create workflow client for agent scheduling
		swClient := simpleworkflow.NewClientWithDB(r.DB(), r.Dialect())
		server.Handlers().SetWorkflowClient(api.NewWorkflowScheduler(swClient))
		server.Handlers().SetRunnerDB(scheduler.NewDebugDB(r.DB(), r.Dialect().DriverName()))
		slog.Info("scheduler workflow client initialized")

		go func() {
			slog.Info("scheduler starting")
			if err := r.Start(context.Background()); err != nil {
				slog.Error("scheduler error", "error", err)
			}
		}()
	} else {
		slog.Info("scheduler disabled (set SCHEDULER_ENABLED=true and SCHEDULER_DATABASE_URL to enable)")
	}

	if err := server.Start(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
