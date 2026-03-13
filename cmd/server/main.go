package main

import (
	"context"
	"log"
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
	log.Printf("Project dir: %s", cfg.ProjectDir)
	log.Printf("Repos root: %s", cfg.ReposRoot)
	log.Printf("Logs root: %s", cfg.LogsRoot)
	log.Printf("Tmp root: %s", cfg.TmpRoot)
	log.Printf("Memory dir: %s", cfg.MemoryDir)
	log.Printf("Max runtime: %ds", cfg.MaxRuntimeSeconds)
	log.Printf("Max concurrent jobs: %d", cfg.MaxConcurrentJobs)
	if len(cfg.AllowedProjects) > 0 {
		log.Printf("Allowed projects: %v", cfg.AllowedProjects)
	} else {
		log.Printf("Allowed projects: all")
	}
	if cfg.API.APIKey != "" {
		log.Printf("API key authentication: enabled")
	}
	if cfg.Agent.Model != "" {
		log.Printf("Agent model: %s", cfg.Agent.Model)
	}
	if cfg.Agent.PromptFile != "" {
		log.Printf("Agent prompt file: %s", cfg.Agent.PromptFile)
	}
	if cfg.Telegram.BotToken != "" {
		log.Printf("Telegram bot: enabled")
	}

	// Seed embedded defaults into memory dir on first run
	if err := tmpl.SeedDefaults(cfg.MemoryDir); err != nil {
		log.Printf("Warning: failed to seed defaults: %v", err)
	}
	// Refresh unmodified defaults to pick up new embedded versions
	if err := tmpl.RefreshDefaults(cfg.MemoryDir); err != nil {
		log.Printf("Warning: failed to refresh defaults: %v", err)
	}

	// Prompt files (AGENT_SYSTEM_PROMPT, AGENT_PROMPT_FILE) are now loaded
	// directly from source on each session via LoadPromptFile — no need to
	// seed copies into the memory directory.

	// Create and start server
	server := api.NewServer(cfg)

	// Conditionally start hybrid runner
	if cfg.Runner.Enabled {
		log.Printf("Runner: enabled (db=%s prefix=%s lease=%ds poll_cap=%ds)",
			cfg.Runner.DatabaseURL, cfg.Runner.TypePrefix, cfg.Runner.LeaseDuration, cfg.Runner.PollCap)
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
		log.Printf("Runner: workflow client initialized for schedule submission")

		go func() {
			log.Printf("Runner: starting hybrid runner")
			if err := r.Start(context.Background()); err != nil {
				log.Printf("Runner error: %v", err)
			}
		}()
	} else {
		log.Printf("Runner: disabled (set RUNNER_SCHEDULER_ENABLED=true and RUNNER_DATABASE_URL to enable scheduled tasks)")
	}

	if err := server.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
