package main

import (
	"context"
	"log"
	"time"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/runner"
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
	} else {
		log.Printf("Agent prompt file: none (using message directly)")
	}
	if cfg.Telegram.BotToken != "" {
		log.Printf("Telegram bot: enabled")
	}

	// Create and start server
	server := api.NewServer(cfg)

	// Conditionally start hybrid runner
	if cfg.Runner.Enabled {
		if cfg.Runner.DatabaseURL == "" {
			log.Fatalf("RUNNER_DATABASE_URL is required when RUNNER_ENABLED=true")
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

		go func() {
			log.Printf("Runner: starting hybrid runner")
			if err := r.Start(context.Background()); err != nil {
				log.Printf("Runner error: %v", err)
			}
		}()
	}

	if err := server.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
