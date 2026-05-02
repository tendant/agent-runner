package api

import (
	"fmt"
	"os"
	"strings"

	"github.com/agent-runner/agent-runner/internal/config"
)

const (
	cmdSetAgent  = "/set-agent"
	cmdSetPrompt = "/set-prompt"
)

// Commander handles chat configuration commands. Zero LLM dependencies —
// safe to call before any model or provider is configured.
type Commander struct {
	cfg      *config.Config
	handlers *Handlers
}

// NewCommander creates a Commander backed by the given config and handlers.
func NewCommander(cfg *config.Config, h *Handlers) *Commander {
	return &Commander{cfg: cfg, handlers: h}
}

// Handle parses text and executes a recognised command. Returns the reply
// and true when a command was handled; returns ("", false) otherwise.
// bareCommands are single-word commands that are safe to accept without a leading slash.
// Multi-word commands (set, set-agent, set-prompt) require the slash to avoid false
// positives on agent instructions that happen to start with those words.
var bareCommands = []string{"help", "config", "bootstrap"}

func (c *Commander) Handle(text string) (string, bool) {
	text = strings.TrimSpace(text)
	lower := strings.ToLower(text)
	// Normalise: accept "config", "help", "bootstrap" without leading slash.
	if !strings.HasPrefix(lower, "/") {
		for _, cmd := range bareCommands {
			if lower == cmd || strings.HasPrefix(lower, cmd+" ") {
				text = "/" + text
				lower = "/" + lower
				break
			}
		}
	}

	switch {
	case lower == "/help":
		return helpText, true
	case lower == "/config":
		return c.handleConfig(), true
	case strings.HasPrefix(lower, "/set "):
		return c.handleSet(text[5:]), true // strip "/set "
	case lower == "/bootstrap" || strings.HasPrefix(lower, "/bootstrap "):
		force := strings.Contains(lower, "force")
		return c.handleBootstrap(force), true
	case lower == "/install-cli" || strings.HasPrefix(lower, "/install-cli "):
		arg := strings.TrimSpace(text[len("/install-cli"):])
		return c.handleInstallCLI(arg), true
	case strings.HasPrefix(lower, cmdSetAgent+" ") || lower == cmdSetAgent:
		content := strings.TrimSpace(text[len(cmdSetAgent):])
		return c.handleSetFile("agent.md", content, true), true
	case strings.HasPrefix(lower, cmdSetPrompt+" ") || lower == cmdSetPrompt:
		content := strings.TrimSpace(text[len(cmdSetPrompt):])
		return c.handleSetFile("prompt.md", content, false), true
	}
	return "", false
}

// handleConfig returns the current configuration state, one key=value per line.
// Sensitive keys (API keys, tokens) are shown only when set; their values are
// never echoed. Keys not present are omitted.
func (c *Commander) handleConfig() string {
	var b strings.Builder

	// CLI always shown — has default.
	cli := c.cfg.Agent.CLI
	if cli == "" {
		cli = "opencode"
	}
	fmt.Fprintf(&b, "cli=%s\n", cli)
	fmt.Fprintf(&b, "cli_installed=%v\n", CLIInstalled(cli))

	if c.cfg.Agent.Provider != "" {
		fmt.Fprintf(&b, "provider=%s\n", c.cfg.Agent.Provider)
	}
	if c.cfg.Agent.Model != "" {
		fmt.Fprintf(&b, "model=%s\n", c.cfg.Agent.Model)
	}

	// Show set API keys (never show values).
	for _, key := range configAPIKeys {
		if os.Getenv(key) != "" {
			fmt.Fprintf(&b, "%s=set\n", key)
		}
	}

	// File state always shown.
	systemPath, promptPath := c.handlers.bootstrapPaths()
	fmt.Fprintf(&b, "agent.md=%s\n", fileState(systemPath))
	fmt.Fprintf(&b, "prompt.md=%s\n", fileState(promptPath))

	// Ready = no credential warnings.
	warnings := bootstrapWarnings(cli, c.cfg.Agent.Provider)
	if len(warnings) == 0 {
		fmt.Fprintf(&b, "ready=true\n")
	} else {
		fmt.Fprintf(&b, "ready=false\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// handleSet parses "KEY VALUE" or "KEY=VALUE" and persists it.
func (c *Commander) handleSet(args string) string {
	key, val, ok := parseSetArgs(args)
	if !ok {
		return "error: usage: /set KEY VALUE  or  /set KEY=VALUE"
	}

	// Persist to .env.local and apply to current process.
	if err := config.SetEnvLocal(key, val); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	os.Setenv(key, val) //nolint:errcheck

	// Update live config + executor for agent-level settings.
	switch key {
	case "AGENT_CLI":
		c.cfg.Agent.CLI = val
		c.handlers.UpdateExecutor()
	case "AGENT_PROVIDER":
		c.cfg.Agent.Provider = val
		c.handlers.UpdateExecutor()
	case "AGENT_MODEL":
		c.cfg.Agent.Model = val
		c.handlers.UpdateExecutor()
	case "AGENT_MAX_TURNS":
		// Let executor.NewExecutor re-read via config; best-effort int parse.
		var n int
		fmt.Sscanf(val, "%d", &n)
		c.cfg.Agent.MaxTurns = n
		c.handlers.UpdateExecutor()
	}

	if isSensitiveKey(key) {
		return "ok " + key
	}
	return fmt.Sprintf("ok %s=%s", key, val)
}

// handleInstallCLI installs the given agent CLI (or the configured default) via npm.
func (c *Commander) handleInstallCLI(arg string) string {
	cli := strings.TrimSpace(arg)
	if cli == "" {
		cli = c.cfg.Agent.CLI
	}
	if cli == "" {
		cli = "opencode"
	}
	if CLIInstalled(cli) {
		return fmt.Sprintf("ok %s already installed", cli)
	}
	out, err := installCLI(cli)
	out = strings.TrimSpace(out)
	if err != nil {
		if out != "" {
			return fmt.Sprintf("error installing %s: %v\n%s", cli, err, out)
		}
		return fmt.Sprintf("error installing %s: %v", cli, err)
	}
	return fmt.Sprintf("ok installed %s\n%s", cli, out)
}

// handleSetFile writes content to the given prompt file (agent.md or prompt.md).
// isSystem=true targets the system-prompt path, isSystem=false targets the prompt-file path.
func (c *Commander) handleSetFile(name, content string, isSystem bool) string {
	if content == "" {
		return fmt.Sprintf("error: usage: %s <content>", map[bool]string{true: cmdSetAgent, false: cmdSetPrompt}[isSystem])
	}
	systemPath, promptPath := c.handlers.bootstrapPaths()
	path := promptPath
	if isSystem {
		path = systemPath
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0644); err != nil {
		return fmt.Sprintf("error: write %s: %v", name, err)
	}
	return fmt.Sprintf("ok wrote %s (%d bytes)", name, len(content))
}

// handleBootstrap creates default agent.md and prompt.md.
func (c *Commander) handleBootstrap(force bool) string {
	systemPath, promptPath := c.handlers.bootstrapPaths()
	results, err := createBootstrapFiles(systemPath, promptPath, force)
	if err != nil {
		return "error: " + err.Error()
	}

	var b strings.Builder
	for _, r := range results {
		if r.created {
			fmt.Fprintf(&b, "created %s\n", r.path)
		} else {
			fmt.Fprintf(&b, "skipped %s (already exists)\n", r.path)
		}
	}

	cli := c.cfg.Agent.CLI
	if cli == "" {
		cli = "opencode"
	}
	warnings := bootstrapWarnings(cli, c.cfg.Agent.Provider)
	if len(warnings) == 0 {
		b.WriteString("ready=true")
	} else {
		b.WriteString("ready=false")
	}
	return b.String()
}

// parseSetArgs parses "KEY VALUE" or "KEY=VALUE" into (key, value, true).
func parseSetArgs(s string) (key, val string, ok bool) {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '='); idx > 0 {
		return strings.TrimSpace(s[:idx]), s[idx+1:], true
	}
	parts := strings.SplitN(s, " ", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
	}
	return "", "", false
}

// isSensitiveKey returns true for keys whose values must not be echoed.
func isSensitiveKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, suffix := range []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// fileState returns "exists" or "missing" for a file path.
func fileState(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "exists"
	}
	return "missing"
}

const helpText = `/help      show this message
/config    show current configuration and readiness
/set KEY VALUE  set a config value (saved to .env.local, survives restart)
           examples:
             /set AGENT_CLI claude
             /set ANTHROPIC_API_KEY <key>   ← auth for claude
             /set OPENAI_API_KEY <key>      ← auth for codex
             /set AGENT_PROVIDER deepseek
             /set AGENT_MODEL deepseek-chat
             /set DEEPSEEK_API_KEY <key>
/install-cli [cli]  install agent CLI (default: configured CLI)
                    e.g. /install-cli claude  /install-cli codex  /install-cli opencode
                    after installing, set the matching API key to authenticate
/bootstrap      create default agent.md and prompt.md (also installs CLI)
/bootstrap force  overwrite existing files
/set-agent <content>   overwrite agent.md with the given content
/set-prompt <content>  overwrite prompt.md with the given content`

// configAPIKeys is the set of provider API keys shown in /config when set.
var configAPIKeys = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"DEEPSEEK_API_KEY",
	"OPENROUTER_API_KEY",
	"GEMINI_API_KEY",
	"GROQ_API_KEY",
	"MISTRAL_API_KEY",
}
