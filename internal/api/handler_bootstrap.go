package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/agent-runner/agent-runner/internal/clisetup"
)

// BootstrapResponse is returned by POST /bootstrap.
type BootstrapResponse struct {
	Created      []string        `json:"created"`
	Skipped      []string        `json:"skipped"`
	Config       BootstrapConfig `json:"config"`
	Warnings     []string        `json:"warnings"`
	Ready        bool            `json:"ready"`
	CLIInstalled bool            `json:"cli_installed"`
	CLIVersion   string          `json:"cli_version,omitempty"`
	CLIOutput    string          `json:"cli_output,omitempty"`
}

// BootstrapConfig summarises the current executor configuration.
type BootstrapConfig struct {
	CLI      string `json:"cli"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
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
	// body is optional — missing or malformed JSON just leaves Force=false
	_ = json.NewDecoder(r.Body).Decode(&req)

	systemPromptPath, promptFilePath := h.bootstrapPaths()

	results, err := clisetup.CreateBootstrapFiles(systemPromptPath, promptFilePath, req.Force)
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := BootstrapResponse{}
	for _, r := range results {
		if r.Created {
			resp.Created = append(resp.Created, r.Path)
		} else {
			resp.Skipped = append(resp.Skipped, r.Path)
		}
	}

	cli := h.config.Agent.CLI
	if cli == "" {
		cli = "opencode"
	}
	resp.Config = BootstrapConfig{
		CLI:      cli,
		Provider: h.config.Agent.Provider,
		Model:    h.config.Agent.Model,
	}

	// Install the CLI if it is not already present.
	if clisetup.CLIInstalled(cli) {
		resp.CLIInstalled = true
	} else {
		out, err := clisetup.InstallCLI(cli)
		resp.CLIOutput = strings.TrimSpace(out)
		if err != nil {
			resp.CLIOutput = fmt.Sprintf("install failed: %v\n%s", err, resp.CLIOutput)
			resp.CLIInstalled = false
		} else {
			resp.CLIInstalled = true
		}
	}
	if resp.CLIInstalled {
		resp.CLIVersion, _ = clisetup.CLIVersion(cli)
	}

	resp.Warnings = clisetup.BootstrapWarnings(cli, h.config.Agent.Provider)
	resp.Ready = len(resp.Warnings) == 0 && resp.CLIInstalled

	h.writeJSON(w, http.StatusOK, resp)
}

// bootstrapPaths returns the system prompt and prompt file paths from config.
// Defaults to memory/agent.md and memory/prompt.md so bootstrap creates and
// reads them in the git-tracked memory directory.
func (h *Handlers) bootstrapPaths() (systemPrompt, promptFile string) {
	systemPrompt = h.config.Agent.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = filepath.Join(h.config.MemoryDir, "agent.md")
	}
	promptFile = h.config.Agent.PromptFile
	if promptFile == "" {
		promptFile = filepath.Join(h.config.MemoryDir, "prompt.md")
	}
	return
}
