package main

import (
	"log"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Log configuration summary
	log.Printf("Projects root: %s", cfg.ProjectsRoot)
	log.Printf("Runs root: %s", cfg.RunsRoot)
	log.Printf("Tmp root: %s", cfg.TmpRoot)
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
	if cfg.Agent.DefaultProject != "" {
		log.Printf("Agent default project: %s", cfg.Agent.DefaultProject)
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
	if err := server.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
