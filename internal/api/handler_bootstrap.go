package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultAgentMD = `# Agent

You are an autonomous software development agent.

## Core Principles

- Complete tasks thoroughly before reporting done
- Commit all changes with descriptive commit messages and push to origin
- Prefer making progress over waiting for perfect information
- Ask for clarification only when the task is genuinely ambiguous

## Memory

Your memory directory is {{MEMORY_DIR}}. Files written there persist across sessions
and are loaded into your context at the start of every session.
**Important:** {{MEMORY_DIR}} is an absolute path. Write files directly into it — do NOT create a subdirectory named "memory" inside it.

Write important information directly to memory files:

- {{MEMORY_DIR}}/user_preferences.md — user name, role, preferences
- {{MEMORY_DIR}}/project_summary.md  — project goals, tech stack, status
- {{MEMORY_DIR}}/decisions.md        — key decisions and rationale

To update your workflow instructions for future sessions, write to {{MEMORY_DIR}}/prompt.md.

Keep entries brief. Append — do not delete existing content unless correcting an error.

## Credentials

API keys and credentials (ANTHROPIC_API_KEY, OPENAI_API_KEY, AWS_*, etc.) are available directly
as environment variables — the server loads .env and .env.local at startup and all subprocesses
inherit them. No file loading is needed.

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
		if err := os.MkdirAll(filepath.Dir(f.path), 0755); err != nil {
			return results, fmt.Errorf("failed to create dir for %s: %w", f.path, err)
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
		cli = "opencode"
	}
	resp.Config = BootstrapConfig{
		CLI:      cli,
		Provider: h.config.Agent.Provider,
		Model:    h.config.Agent.Model,
	}

	// Install the CLI if it is not already present.
	if CLIInstalled(cli) {
		resp.CLIInstalled = true
	} else {
		out, err := installCLI(cli)
		resp.CLIOutput = strings.TrimSpace(out)
		if err != nil {
			resp.CLIOutput = fmt.Sprintf("install failed: %v\n%s", err, resp.CLIOutput)
			resp.CLIInstalled = false
		} else {
			resp.CLIInstalled = true
		}
	}
	if resp.CLIInstalled {
		resp.CLIVersion = cliVersion(cli)
	}

	resp.Warnings = BootstrapWarnings(cli, h.config.Agent.Provider)
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


// CLIInstalled reports whether the given CLI binary is present in PATH.
func CLIInstalled(cli string) bool {
	if cli == "" {
		cli = "opencode"
	}
	_, err := exec.LookPath(cli)
	return err == nil
}

// ensureNodeScript is a shell snippet prepended to npm-based installs.
// It detects the package manager and installs Node.js + npm if they are absent.
const ensureNodeScript = `set -e
if ! command -v npm >/dev/null 2>&1; then
  echo "npm not found, installing Node.js..."
  if command -v apk >/dev/null 2>&1; then
    apk add --no-cache nodejs npm
  elif command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq nodejs npm
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y nodejs npm
  elif command -v yum >/dev/null 2>&1; then
    yum install -y nodejs npm
  elif command -v brew >/dev/null 2>&1; then
    brew install node
  else
    echo "cannot install npm: no supported package manager found (apk, apt, dnf, yum, brew)" >&2; exit 1
  fi
fi
`

// cliVersion returns the installed version string for the given CLI binary,
// or an empty string if it cannot be determined.
func cliVersion(cli string) string {
	if cli == "" {
		cli = "opencode"
	}
	// Resolve to the full path so the version call uses the same binary
	// that CLIInstalled() found, regardless of PATH ordering.
	path, err := exec.LookPath(cli)
	if err != nil {
		slog.Warn("cliVersion: binary not found", "cli", cli, "error", err)
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// Allow AppImage binaries (e.g. opencode on Linux) to run without FUSE
	// by extracting instead of mounting.
	cmd.Env = append(os.Environ(), "APPIMAGE_EXTRACT_AND_RUN=1")
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("cliVersion: --version failed", "cli", cli, "path", path, "error", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// installCLI installs the given agent CLI backend.
// opencode is downloaded from GitHub releases; claude and codex use npm.
// Returns combined output and any error.
func installCLI(cli string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	switch cli {
	case "opencode":
		// Download the latest binary from GitHub releases (Linux and macOS).
		// Falls back to ~/bin if /usr/local/bin is not writable (macOS without sudo).
		script := `set -e
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
URL=$(curl -fsSL https://api.github.com/repos/sst/opencode/releases/latest \
  | grep browser_download_url | grep "\"$OS\"" | grep "$ARCH" | head -1 \
  | cut -d'"' -f4)
if [ -z "$URL" ]; then
  # fallback: grep without quotes (older releases use different naming)
  URL=$(curl -fsSL https://api.github.com/repos/sst/opencode/releases/latest \
    | grep browser_download_url | grep "$OS" | grep "$ARCH" | head -1 \
    | cut -d'"' -f4)
fi
if [ -z "$URL" ]; then
  echo "no release binary found for $OS/$ARCH" >&2; exit 1
fi
INSTALL_DIR=/usr/local/bin
if [ ! -w "$INSTALL_DIR" ]; then
  INSTALL_DIR="$HOME/bin"
  mkdir -p "$INSTALL_DIR"
  echo "note: /usr/local/bin not writable, installing to $INSTALL_DIR"
fi
curl -fsSL "$URL" -o "$INSTALL_DIR/opencode"
chmod +x "$INSTALL_DIR/opencode"
echo "installed to $INSTALL_DIR/opencode"`
		cmd = exec.CommandContext(ctx, "sh", "-c", script)
	case "codex":
		cmd = exec.CommandContext(ctx, "sh", "-c", ensureNodeScript+"npm install -g @openai/codex")
	default: // claude
		cmd = exec.CommandContext(ctx, "sh", "-c", ensureNodeScript+"npm install -g @anthropic-ai/claude-code")
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// cliInstallHint returns a human-readable install hint for /config output.
func cliInstallHint(cli string) string {
	switch cli {
	case "opencode":
		return "github.com/sst/opencode (latest binary, linux/macos)"
	case "codex":
		return "npm install -g @openai/codex (auto-installs Node.js if needed)"
	default:
		return "npm install -g @anthropic-ai/claude-code (auto-installs Node.js if needed)"
	}
}

// BootstrapWarnings returns human-readable warnings for missing credentials.
func BootstrapWarnings(cli, provider string) []string {
	var w []string
	p := strings.ToLower(provider)

	switch cli {
	case "claude":
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_BASE_URL") == "" {
			w = append(w, "claude backend requires ANTHROPIC_API_KEY (or ANTHROPIC_BASE_URL for local models)")
		}
	case "codex":
		if os.Getenv("OPENAI_API_KEY") == "" && !codexHasOAuthCredentials() {
			w = append(w, "codex backend requires OPENAI_API_KEY or /auth codex")
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

// codexHasOAuthCredentials returns true if codex has OAuth credentials from
// a previous `codex login --device-auth` run (stored in ~/.codex/auth.json).
func codexHasOAuthCredentials() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".codex", "auth.json"))
	return err == nil
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
