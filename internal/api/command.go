package api

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/agent-runner/agent-runner/internal/config"
)

const (
	cmdSetAgent  = "/set-agent"
	cmdSetPrompt = "/set-prompt"
)

// Commander handles chat configuration commands. Zero LLM dependencies —
// safe to call before any model or provider is configured.
type Commander struct {
	cfg        *config.Config
	handlers   *Handlers
	authMu     sync.Mutex   // guards against concurrent /auth flows
	authCancel func()       // non-nil while an auth flow is running
}

// NewCommander creates a Commander backed by the given config and handlers.
func NewCommander(cfg *config.Config, h *Handlers) *Commander {
	return &Commander{cfg: cfg, handlers: h}
}

// Handle parses text and executes a recognised command. Returns the reply
// and true when a command was handled; returns ("", false) otherwise.
// send is an optional callback for async messages (needed by /auth); pass nil
// when async notifications are not available (e.g. REST API callers).
// bareCommands are single-word commands that are safe to accept without a leading slash.
// Multi-word commands (set, set-agent, set-prompt) require the slash to avoid false
// positives on agent instructions that happen to start with those words.
var bareCommands = []string{"help", "config", "bootstrap"}

func (c *Commander) Handle(text string, send func(string)) (string, bool) {
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
	case lower == "/auth cancel":
		return c.handleAuthCancel(), true
	case lower == "/auth" || strings.HasPrefix(lower, "/auth "):
		arg := strings.TrimSpace(text[len("/auth"):])
		return c.handleAuth(arg, send), true
	}
	return "", false
}

// handleAuth starts a CLI auth flow in a background goroutine and relays
// progress (including the auth URL) to send. Only 'claude' is supported.
func (c *Commander) handleAuth(arg string, send func(string)) string {
	if send == nil {
		return "error: /auth is only available via chat (Telegram, WeChat, stream)"
	}
	cli := strings.TrimSpace(arg)
	if cli == "" {
		cli = c.cfg.Agent.CLI
	}
	if cli == "" || cli == "opencode" {
		return "opencode authenticates via API keys — use /set <PROVIDER>_API_KEY <key> instead"
	}
	if cli != "claude" && cli != "codex" {
		return fmt.Sprintf("error: /auth only supports 'claude' and 'codex' (got %q)", cli)
	}
	if !CLIInstalled(cli) {
		return fmt.Sprintf("error: %s is not installed — run /install-cli %s first", cli, cli)
	}
	if !c.authMu.TryLock() {
		return "an auth flow is already in progress — use /auth cancel to stop it"
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.authCancel = cancel
	go func() {
		defer func() {
			c.authCancel = nil
			c.authMu.Unlock()
		}()
		runCLIAuthFlowCtx(ctx, cli, send)
	}()
	return "Starting " + cli + " auth — open the URL when it appears in chat... (/auth cancel to stop)"
}

// handleAuthCancel cancels any running auth flow.
func (c *Commander) handleAuthCancel() string {
	c.authMu.Lock()
	cancel := c.authCancel
	c.authMu.Unlock()
	if cancel == nil {
		return "no auth flow is running"
	}
	cancel()
	return "auth flow cancelled"
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

	b.WriteString("**Configuration**\n\n")
	fmt.Fprintf(&b, "**cli:** %s (%s)\n", cli, map[bool]string{true: "installed", false: "not installed"}[CLIInstalled(cli)])

	if c.cfg.Agent.Provider != "" {
		fmt.Fprintf(&b, "**provider:** %s\n", c.cfg.Agent.Provider)
	}
	if c.cfg.Agent.Model != "" {
		fmt.Fprintf(&b, "**model:** %s\n", c.cfg.Agent.Model)
	}

	// Show set API keys (never show values).
	for _, key := range configAPIKeys {
		if os.Getenv(key) != "" {
			fmt.Fprintf(&b, "**%s:** set\n", key)
		}
	}

	// File state always shown.
	systemPath, promptPath := c.handlers.bootstrapPaths()
	fmt.Fprintf(&b, "**agent.md:** %s\n", fileState(systemPath))
	fmt.Fprintf(&b, "**prompt.md:** %s\n", fileState(promptPath))

	// Ready = no credential warnings.
	warnings := bootstrapWarnings(cli, c.cfg.Agent.Provider)
	if len(warnings) == 0 {
		b.WriteString("**ready:** ✓")
	} else {
		b.WriteString("**ready:** ✗")
	}

	return b.String()
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

const helpText = `**Agent Runner Commands**

**/config** — show current configuration and readiness

**/set** _KEY VALUE_ — set a config value (saved to .env.local, survives restart)
Examples: /set AGENT\_CLI claude · /set ANTHROPIC\_API\_KEY \<key\> · /set DEEPSEEK\_API\_KEY \<key\>

**/install-cli** _[cli]_ — install agent CLI (claude / codex / opencode)

**/bootstrap** — create default agent.md and prompt.md
**/bootstrap force** — overwrite existing files

**/auth** _[cli]_ — start OAuth login flow via chat (claude or codex)
**/auth cancel** — stop an in-progress auth flow

**/set-agent** _\<content\>_ — overwrite agent.md
**/set-prompt** _\<content\>_ — overwrite prompt.md`

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
