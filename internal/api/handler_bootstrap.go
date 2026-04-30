package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const defaultAgentMD = `# Agent

You are an autonomous software development agent. Your working directory is ` + "`workspace/`" + `.

## Core Principles

- Complete tasks thoroughly before reporting done
- Commit all changes with descriptive commit messages
- Prefer making progress over waiting for perfect information
- Ask for clarification only when the task is genuinely ambiguous

## When Done

Report what you did in 2-3 sentences. List files changed and key commands run.
`

const defaultPromptMD = `# Task Workflow

1. Read the task carefully and identify what needs to change
2. Explore relevant files and understand the current state
3. Make the changes
4. Test if applicable
5. Commit with a descriptive message
6. Report what was done
`

// BootstrapResponse is returned by POST /bootstrap.
type BootstrapResponse struct {
	Created  []string        `json:"created"`
	Skipped  []string        `json:"skipped"`
	Config   BootstrapConfig `json:"config"`
	Warnings []string        `json:"warnings"`
	Ready    bool            `json:"ready"`
}

// BootstrapConfig summarises the current executor configuration.
type BootstrapConfig struct {
	CLI      string `json:"cli"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// bootstrapFileResult records what happened to one file during bootstrap.
type bootstrapFileResult struct {
	path    string
	created bool // false = skipped (already exists)
}

// createBootstrapFiles writes default agent.md and prompt.md when they are
// missing. With force=true, existing files are overwritten. Returns one result
// per file and any write error encountered.
func createBootstrapFiles(systemPromptPath, promptFilePath string, force bool) ([]bootstrapFileResult, error) {
	files := []struct {
		path    string
		content string
	}{
		{systemPromptPath, defaultAgentMD},
		{promptFilePath, defaultPromptMD},
	}

	var results []bootstrapFileResult
	for _, f := range files {
		if !force {
			if _, err := os.Stat(f.path); err == nil {
				results = append(results, bootstrapFileResult{path: f.path, created: false})
				continue
			}
		}
		if err := os.WriteFile(f.path, []byte(f.content), 0644); err != nil {
			return results, fmt.Errorf("failed to write %s: %w", f.path, err)
		}
		results = append(results, bootstrapFileResult{path: f.path, created: true})
	}
	return results, nil
}

// HandleBootstrap creates default agent.md and prompt.md templates when they
// are missing. It requires no LLM or model — safe to call before any provider
// is configured.
//
// POST /bootstrap
// Optional JSON body: {"force": true}  — overwrite existing files
func (h *Handlers) HandleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Force bool `json:"force"`
	}
	// ignore decode errors — body is optional
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

	systemPromptPath, promptFilePath := h.bootstrapPaths()

	results, err := createBootstrapFiles(systemPromptPath, promptFilePath, req.Force)
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := BootstrapResponse{}
	for _, r := range results {
		if r.created {
			resp.Created = append(resp.Created, r.path)
		} else {
			resp.Skipped = append(resp.Skipped, r.path)
		}
	}

	cli := h.config.Agent.CLI
	if cli == "" {
		cli = "claude"
	}
	resp.Config = BootstrapConfig{
		CLI:      cli,
		Provider: h.config.Agent.Provider,
		Model:    h.config.Agent.Model,
	}
	resp.Warnings = bootstrapWarnings(cli, h.config.Agent.Provider)
	resp.Ready = len(resp.Warnings) == 0

	h.writeJSON(w, http.StatusOK, resp)
}

// bootstrapPaths returns the system prompt and prompt file paths from config,
// falling back to conventional names.
func (h *Handlers) bootstrapPaths() (systemPrompt, promptFile string) {
	systemPrompt = h.config.Agent.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = "./agent.md"
	}
	promptFile = h.config.Agent.PromptFile
	if promptFile == "" {
		promptFile = "./prompt.md"
	}
	return
}

// bootstrapWarnings returns human-readable warnings for missing credentials.
func bootstrapWarnings(cli, provider string) []string {
	var w []string
	p := strings.ToLower(provider)

	switch cli {
	case "claude":
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_BASE_URL") == "" {
			w = append(w, "claude backend requires ANTHROPIC_API_KEY (or ANTHROPIC_BASE_URL for local models)")
		}
	case "codex":
		if os.Getenv("OPENAI_API_KEY") == "" {
			w = append(w, "codex backend requires OPENAI_API_KEY")
		}
	case "opencode":
		key := providerEnvKey(p)
		if key != "" && os.Getenv(key) == "" {
			w = append(w, "opencode/"+p+" requires "+key)
		} else if p == "" {
			// No provider set — opencode will use its own default; warn if no common key is set.
			if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" {
				w = append(w, "no provider configured and no API key found; set AGENT_PROVIDER + the matching *_API_KEY")
			}
		}
	}
	return w
}

// providerEnvKey returns the expected API key env var for a known opencode provider.
func providerEnvKey(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	case "gemini", "google":
		return "GEMINI_API_KEY"
	case "groq":
		return "GROQ_API_KEY"
	case "mistral":
		return "MISTRAL_API_KEY"
	case "ollama":
		return "" // no key required
	default:
		return strings.ToUpper(provider) + "_API_KEY"
	}
}
