package main

import (
	"flag"
	"log"
	"os"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration
	var cfg *config.Config
	var err error

	if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
		log.Printf("Config file %s not found, using defaults", *configPath)
		cfg = config.DefaultConfig()
	} else {
		cfg, err = config.Load(*configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
		log.Printf("Loaded config from %s", *configPath)
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

	// Create and start server
	server := api.NewServer(cfg)
	if err := server.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
