package api

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"strconv"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/logging"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
	"github.com/agent-runner/agent-runner/internal/textutil"
)

const (
	cmdSetAgent  = "/set-agent"
	cmdSetPrompt = "/set-prompt"
	cmdMemory    = "/memory"
	cmdRepo      = "/repo"
)

// Commander handles chat configuration commands. Zero LLM dependencies —
// safe to call before any model or provider is configured.
type Commander struct {
	cfg        *config.Config
	handlers   *Handlers
	authMu     sync.Mutex // guards against concurrent /auth flows
	authCancel func()     // non-nil while an auth flow is running
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
var bareCommands = []string{"help", "config", "status", "sessions", "bootstrap", "logs"}

// Handle parses text and executes a recognised command.
// Returns the reply text, an optional session ID (non-empty when the command
// started an agent session), and true when a command was handled.
// send is an optional callback for async messages (needed by /auth); pass nil
// when async notifications are not available (e.g. REST API callers).
func (c *Commander) Handle(text string, send func(string)) (reply, sessionID string, handled bool) {
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

	r := func(s string) (string, string, bool) { return s, "", true }

	// "Update prompt:" / "update prompt " — write prompt and optionally start session.
	if strings.HasPrefix(lower, "update prompt:") || strings.HasPrefix(lower, "update prompt ") ||
		strings.HasPrefix(lower, "/update-prompt:") || strings.HasPrefix(lower, "/update-prompt ") {
		body := strings.TrimSpace(text[strings.IndexByte(text, ':')+1:])
		if strings.HasPrefix(lower, "/update-prompt ") {
			body = strings.TrimSpace(text[len("/update-prompt"):])
		}
		reply, sid := c.handleUpdatePrompt(body)
		return reply, sid, true
	}

	// knownCommands maps each recognised slash-command name to its usage string.
	// Any input whose first word appears here but doesn't match a full case
	// returns the specific usage hint instead of falling through to the
	// gateway's generic "Unknown command" response.
	knownCommands := map[string]string{
		"/help":          "usage: /help",
		"/config":        "usage: /config",
		"/status":        "usage: /status",
		"/sessions":      "usage: /sessions",
		"/set":           "usage: /set KEY VALUE  or  /set KEY=VALUE",
		"/bootstrap":     "usage: /bootstrap [force]",
		"/install-cli":   "usage: /install-cli [claude|codex|opencode] [force]",
		cmdSetAgent:      "usage: " + cmdSetAgent + " <content>",
		cmdSetPrompt:     "usage: " + cmdSetPrompt + " <content>",
		"/auth":          "usage: /auth [claude|codex] — only available via chat",
		cmdMemory:        "usage: " + cmdMemory + " [show|clear|git <url>]",
		cmdRepo:          "usage: " + cmdRepo + " [add|remove|list] <url>",
		"/stop":          "usage: /stop [session-id]",
		"/logs":          "usage: /logs [session-id-prefix]",
		"/update-prompt": "usage: /update-prompt <content>",
	}

	switch {
	case lower == "/help":
		return r(helpText)
	case lower == "/config":
		return r(c.handleConfig())
	case lower == "/status":
		return r(c.handleStatus())
	case lower == "/sessions":
		return r(c.handleSessions())
	case strings.HasPrefix(lower, "/set ") || lower == "/set":
		return r(c.handleSet(strings.TrimSpace(text[4:]))) // strip "/set"
	case lower == "/bootstrap" || strings.HasPrefix(lower, "/bootstrap "):
		force := strings.Contains(lower, "force")
		return r(c.handleBootstrap(force))
	case lower == "/install-cli" || strings.HasPrefix(lower, "/install-cli "):
		arg := strings.TrimSpace(text[len("/install-cli"):])
		force := strings.Contains(strings.ToLower(arg), "force")
		arg = strings.TrimSpace(strings.ReplaceAll(strings.ToLower(arg), "force", ""))
		return r(c.handleInstallCLI(arg, force))
	case strings.HasPrefix(lower, cmdSetAgent+" ") || lower == cmdSetAgent:
		content := strings.TrimSpace(text[len(cmdSetAgent):])
		return r(c.handleSetFile("agent.md", content, true))
	case strings.HasPrefix(lower, cmdSetPrompt+" ") || lower == cmdSetPrompt:
		content := strings.TrimSpace(text[len(cmdSetPrompt):])
		return r(c.handleSetFile("prompt.md", content, false))
	case lower == "/auth cancel":
		return r(c.handleAuthCancel())
	case lower == "/auth" || strings.HasPrefix(lower, "/auth "):
		arg := strings.TrimSpace(text[len("/auth"):])
		return r(c.handleAuth(arg, send))
	case lower == cmdMemory || strings.HasPrefix(lower, cmdMemory+" "):
		arg := strings.TrimSpace(text[len(cmdMemory):])
		return r(c.handleMemory(arg))
	case lower == cmdRepo || strings.HasPrefix(lower, cmdRepo+" "):
		arg := strings.TrimSpace(text[len(cmdRepo):])
		return r(c.handleRepo(arg))
	case strings.HasPrefix(lower, "/stop ") || lower == "/stop":
		arg := ""
		if len(text) > 6 {
			arg = strings.TrimSpace(text[6:])
		}
		return r(c.handleStop(arg))
	case strings.HasPrefix(lower, "/logs ") || lower == "/logs":
		arg := ""
		if len(text) > 5 {
			arg = strings.TrimSpace(text[5:])
		}
		return r(c.handleLogs(arg))
	}

	// If the first word is a known command, the user typed it with wrong
	// arguments — return the specific usage hint rather than "Unknown command".
	cmdWord := strings.SplitN(lower, " ", 2)[0]
	if usage, ok := knownCommands[cmdWord]; ok {
		return "error: " + usage, "", true
	}
	return "", "", false
}

// handleAuth starts a CLI auth flow in a background goroutine and relays
// progress (including the auth URL) to send.
func (c *Commander) handleAuth(arg string, send func(string)) string {
	if send == nil {
		return "error: /auth is only available via chat (Telegram, WeChat, stream)"
	}
	cli := strings.TrimSpace(arg)
	msg, err := c.startAuthFlow(cli, send)
	if err != nil {
		return "error: " + err.Error()
	}
	return msg
}

// validateAuth resolves and validates the CLI name for an auth flow.
func (c *Commander) validateAuth(cli string) (string, error) {
	if cli == "" {
		cli = c.cfg.Agent.CLI
	}
	if cli == "" || cli == "opencode" {
		return "", fmt.Errorf("opencode authenticates via API keys — use /set <PROVIDER>_API_KEY <key> instead")
	}
	if cli != "claude" && cli != "codex" {
		return "", fmt.Errorf("/auth only supports 'claude' and 'codex' (got %q)", cli)
	}
	if !CLIInstalled(cli) {
		return "", fmt.Errorf("%s is not installed — run /install-cli %s first", cli, cli)
	}
	return cli, nil
}

// registerAuthCancel locks the auth mutex and stores cancel for /auth cancel.
// Returns an error if another flow is already running.
func (c *Commander) registerAuthCancel(cancel context.CancelFunc) error {
	if !c.authMu.TryLock() {
		return fmt.Errorf("an auth flow is already in progress — use /auth cancel to stop it")
	}
	c.authCancel = cancel
	return nil
}

// releaseAuthCancel clears the stored cancel and unlocks the auth mutex.
func (c *Commander) releaseAuthCancel() {
	c.authCancel = nil
	c.authMu.Unlock()
}

// startAuthFlow starts an async auth flow for chat channels (Telegram, WeChat, stream).
func (c *Commander) startAuthFlow(cli string, send func(string)) (string, error) {
	cli, err := c.validateAuth(cli)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.registerAuthCancel(cancel); err != nil {
		cancel()
		return "", err
	}
	go func() {
		defer c.releaseAuthCancel()
		if err := runCLIAuthFlowCtx(ctx, cli, send); err != nil && ctx.Err() == nil {
			send("auth failed: " + err.Error())
		}
	}()
	return "Starting " + cli + " auth — open the URL when it appears... (/auth cancel to stop)", nil
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

// handleStatus returns the current runtime state of the bot.
func (c *Commander) handleStatus() string {
	var b strings.Builder
	b.WriteString("**Status**\n\n")

	// Active sessions
	sessions := c.handlers.agentManager.ListActiveSessions()
	var running, waiting []*agent.Session
	for _, s := range sessions {
		if s.Status == agent.SessionStatusRunning || s.Status == agent.SessionStatusStopping {
			running = append(running, s)
		} else if s.Status == agent.SessionStatusQueued {
			waiting = append(waiting, s)
		}
	}

	if len(running) == 0 {
		b.WriteString("**agent:** idle\n")
	} else {
		for _, s := range running {
			preview := s.Message
			preview = textutil.Truncate(preview, 60)
			fmt.Fprintf(&b, "**agent:** %s — %q [iter %d/%d]\n",
				s.Status, preview, s.CurrentIteration, s.MaxIterations)
		}
	}
	fmt.Fprintf(&b, "**queued:** %d\n", len(waiting))

	// Last completed session
	if last := c.handlers.agentManager.LastCompletedSession(); last != nil {
		ago := time.Since(*last.CompletedAt).Round(time.Second)
		preview := last.Message
		preview = textutil.Truncate(preview, 50)
		switch last.Status {
		case agent.SessionStatusCompleted:
			fmt.Fprintf(&b, "**last:** completed ✓ %s ago — %q\n", ago, preview)
		case agent.SessionStatusStopped:
			reason := last.Error
			if reason == "" {
				reason = "stopped by user"
			}
			fmt.Fprintf(&b, "**last:** stopped ⏹ %s ago — %s\n", ago, reason)
		default:
			errMsg := last.Error
			errMsg = textutil.Truncate(errMsg, 60)
			fmt.Fprintf(&b, "**last:** failed ✗ %s ago — %s\n", ago, errMsg)
		}
	}

	// CLI
	cli := c.cfg.Agent.CLI
	if cli == "" {
		cli = "opencode"
	}
	if CLIInstalled(cli) {
		fmt.Fprintf(&b, "**cli:** %s ✓\n", cli)
	} else {
		fmt.Fprintf(&b, "**cli:** %s ✗ (not installed — /install-cli)\n", cli)
	}

	// Memory git state
	if _, err := os.Stat(filepath.Join(c.cfg.MemoryDir, ".git")); err == nil {
		if out, err := exec.Command("git", "-C", c.cfg.MemoryDir, "remote", "get-url", "origin").Output(); err == nil {
			fmt.Fprintf(&b, "**memory:** git ✓ → %s\n", strings.TrimSpace(string(out)))
		} else {
			b.WriteString("**memory:** git (no remote)\n")
		}
	}

	// Issues
	warnings := BootstrapWarnings(cli, c.cfg.Agent.Provider)
	if len(warnings) == 0 {
		b.WriteString("**ready:** ✓")
	} else {
		b.WriteString("**ready:** ✗\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "  · %s\n", w)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// handleSessions lists all active and recent sessions with their IDs and status.
func (c *Commander) handleSessions() string {
	sessions := c.handlers.agentManager.ListSessions(20)
	if len(sessions) == 0 {
		return "no sessions"
	}

	var b strings.Builder
	b.WriteString("**Sessions**\n\n")
	for _, s := range sessions {
		preview := s.Message
		preview = textutil.Truncate(preview, 50)
		switch s.Status {
		case agent.SessionStatusRunning, agent.SessionStatusStopping:
			fmt.Fprintf(&b, "🔵 `%s` — %s [iter %d/%d] %q\n",
				s.ID, s.Status, s.CurrentIteration, s.MaxIterations, preview)
		case agent.SessionStatusQueued:
			fmt.Fprintf(&b, "⏳ `%s` — queued %q\n", s.ID, preview)
		case agent.SessionStatusCompleted:
			ago := ""
			if s.CompletedAt != nil {
				ago = " " + time.Since(*s.CompletedAt).Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(&b, "✅ `%s` — completed%s %q\n", s.ID, ago, preview)
		case agent.SessionStatusFailed:
			ago := ""
			if s.CompletedAt != nil {
				ago = " " + time.Since(*s.CompletedAt).Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(&b, "❌ `%s` — failed%s %q\n", s.ID, ago, preview)
		case agent.SessionStatusStopped:
			ago := ""
			if s.CompletedAt != nil {
				ago = " " + time.Since(*s.CompletedAt).Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(&b, "⏹️ `%s` — stopped%s %q\n", s.ID, ago, preview)
		default:
			fmt.Fprintf(&b, "   `%s` — %s %q\n", s.ID, s.Status, preview)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleLogs shows recent agent session logs from the logs directory.
// arg can be empty (show last 3), a number n (show last n), or a session-ID
// prefix (show that session's log).
func (c *Commander) handleLogs(arg string) string {
	arg = strings.TrimSpace(arg)
	logsRoot := c.handlers.config.LogsRoot
	if logsRoot == "" {
		return "error: LOGS_ROOT is not configured"
	}

	rl := logging.NewRunLogger(logsRoot)

	// Determine whether arg is a count or a session-ID prefix.
	n := 3
	prefix := ""
	if arg != "" {
		if v, err := strconv.Atoi(arg); err == nil {
			n = v
		} else {
			prefix = arg
			n = 0
		}
	}

	summaries, err := rl.ListRecentAgentLogs(n, prefix)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(summaries) == 0 {
		if prefix != "" {
			return fmt.Sprintf("no log found matching %q", prefix)
		}
		return "no agent logs found"
	}

	var b strings.Builder
	b.WriteString("**Recent Agent Logs**\n\n")
	for _, s := range summaries {
		preview := s.Message
		preview = textutil.Truncate(preview, 60)
		sid := s.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		icon := "✅"
		switch s.Status {
		case "failed":
			icon = "❌"
		case "stopped":
			icon = "⏹️"
		}
		fmt.Fprintf(&b, "%s `%s` — %s", icon, sid, s.Timestamp)
		if s.Duration != "" {
			fmt.Fprintf(&b, " (%s)", s.Duration)
		}
		b.WriteString("\n")
		if preview != "" {
			fmt.Fprintf(&b, "  %q\n", preview)
		}
		if s.Iterations != "" {
			fmt.Fprintf(&b, "  iterations: %s\n", s.Iterations)
		}
		if s.Cost != "" {
			fmt.Fprintf(&b, "  cost: %s\n", s.Cost)
		}
		if s.Error != "" {
			errSnip := s.Error
			errSnip = textutil.Truncate(errSnip, 80)
			fmt.Fprintf(&b, "  error: %s\n", errSnip)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleStop signals a session to stop by partial or full ID.
func (c *Commander) handleStop(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "usage: /stop <session-id>"
	}
	sessions := c.handlers.agentManager.ListSessions(0)
	for _, s := range sessions {
		if s.ID == arg || strings.HasPrefix(s.ID, "agent-"+arg) || strings.HasPrefix(s.ID, arg) {
			if s.Status != agent.SessionStatusRunning && s.Status != agent.SessionStatusQueued {
				return fmt.Sprintf("session %s is not running (status: %s)", s.ID, s.Status)
			}
			if err := c.handlers.agentManager.StopSession(s.ID); err != nil {
				return fmt.Sprintf("error stopping %s: %v", s.ID, err)
			}
			return fmt.Sprintf("stop requested for %s", s.ID)
		}
	}
	return fmt.Sprintf("no session found matching %q", arg)
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
	if v, err := cliVersion(cli); err != nil {
		fmt.Fprintf(&b, "**cli:** %s (installed, version check failed: %s)\n", cli, err)
	} else if v != "" {
		fmt.Fprintf(&b, "**cli:** %s %s (installed)\n", cli, v)
	} else if CLIInstalled(cli) {
		fmt.Fprintf(&b, "**cli:** %s (installed)\n", cli)
	} else {
		fmt.Fprintf(&b, "**cli:** %s (not installed — /install-cli)\n", cli)
	}

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

	// Repo git credentials (shown when set; token value hidden).
	if c.cfg.GitToken != "" {
		b.WriteString("**GIT_TOKEN:** set\n")
	}
	if v := c.cfg.GitSSHKey; v != "" {
		fmt.Fprintf(&b, "**GIT_SSH_KEY:** %s\n", v)
	}

	// Memory git credentials (shown when set; values hidden).
	if os.Getenv("MEMORY_GIT_TOKEN") != "" {
		b.WriteString("**MEMORY_GIT_TOKEN:** set\n")
	}
	if v := os.Getenv("MEMORY_GIT_USER"); v != "" {
		fmt.Fprintf(&b, "**MEMORY_GIT_USER:** %s\n", v)
	}
	if v := os.Getenv("MEMORY_GIT_SSH_KEY"); v != "" {
		fmt.Fprintf(&b, "**MEMORY_GIT_SSH_KEY:** %s\n", v)
	}

	// File state always shown.
	systemPath, promptPath := c.handlers.bootstrapPaths()
	fmt.Fprintf(&b, "**agent.md:** %s\n", fileState(systemPath))
	fmt.Fprintf(&b, "**prompt.md:** %s\n", fileState(promptPath))

	// Ready = no credential warnings.
	warnings := BootstrapWarnings(cli, c.cfg.Agent.Provider)
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

	// Apply to the process env first — OS env wins the config merge, so the
	// reload below picks the new value up.
	if err := os.Setenv(key, val); err != nil {
		slog.Warn("set: failed to apply env var to process", "key", key, "error", err)
	}

	// Reload the shared config in place — every consumer sees the change with
	// the same parsing (splits, aliases, legacy modes) as startup — then
	// rebuild the executor and LLM clients derived from it.
	if err := c.cfg.ReloadFromEnv(); err != nil {
		return fmt.Sprintf("applied %s, but config reload failed: %v — fix the value or restart to apply", key, err)
	}
	c.handlers.RefreshRuntime()

	// Persist to DATA_DIR/.env.local (path refreshed by the reload above) so
	// the change survives restarts.
	if err := config.SetEnvLocal(key, val); err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	if isSensitiveKey(key) {
		return "ok " + key
	}
	return fmt.Sprintf("ok %s=%s", key, val)
}

// handleInstallCLI installs the given agent CLI (or the configured default) via npm.
// With force=true, reinstalls even when already present.
func (c *Commander) handleInstallCLI(arg string, force bool) string {
	cli := strings.TrimSpace(arg)
	if cli == "" {
		cli = c.cfg.Agent.CLI
	}
	if cli == "" {
		cli = "opencode"
	}
	if !force && CLIInstalled(cli) {
		if v, err := cliVersion(cli); err != nil {
			return fmt.Sprintf("ok %s already installed (version check failed: %s)", cli, err)
		} else if v != "" {
			return fmt.Sprintf("ok %s already installed (%s)", cli, v)
		}
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
	if v, err := cliVersion(cli); err != nil {
		return fmt.Sprintf("ok installed %s (version check failed: %s)\n%s", cli, err, out)
	} else if v != "" {
		return fmt.Sprintf("ok installed %s (%s)\n%s", cli, v, out)
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
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Sprintf("error: create dir for %s: %v", name, err)
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
	warnings := BootstrapWarnings(cli, c.cfg.Agent.Provider)
	if len(warnings) == 0 {
		b.WriteString("ready=true")
	} else {
		b.WriteString("ready=false")
	}
	return b.String()
}

// handleUpdatePrompt parses an "Update prompt:" body that may contain
// "System:" and "User:" sections. The System section is written to prompt.md;
// the User section (if present) is sent as an agent task.
//
// Accepted formats:
//
//	Update prompt: <content>           — just update prompt.md
//	Update prompt:                     — same with body below
//	System: <system content>           — written to prompt.md
//	User: <task>                       — started as agent session
func (c *Commander) handleUpdatePrompt(body string) (reply, sessionID string) {
	systemContent, userContent := parseSystemUser(body)

	// Write the system / prompt content if provided.
	if systemContent != "" {
		_, promptPath := c.handlers.bootstrapPaths()
		if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err == nil {
			_ = os.WriteFile(promptPath, []byte(systemContent+"\n"), 0644)
		}
	} else if userContent == "" {
		// No sections — treat the whole body as prompt content.
		if body != "" {
			_, promptPath := c.handlers.bootstrapPaths()
			if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err == nil {
				_ = os.WriteFile(promptPath, []byte(body+"\n"), 0644)
			}
		}
	}

	if systemContent != "" && userContent == "" {
		return fmt.Sprintf("ok wrote prompt.md (%d bytes)", len(systemContent)), ""
	}
	if systemContent == "" && userContent == "" {
		if body == "" {
			return "error: usage: Update prompt: <content>", ""
		}
		return fmt.Sprintf("ok wrote prompt.md (%d bytes)", len(body)), ""
	}

	// Start an agent session with the User section.
	sid, err := c.agentStarter().StartAgent(userContent, "commander")
	if err != nil {
		return fmt.Sprintf("ok wrote prompt.md (%d bytes); error starting session: %v", len(systemContent), err), ""
	}
	return fmt.Sprintf("ok wrote prompt.md (%d bytes); starting session %s", len(systemContent), sid), sid
}

// parseSystemUser splits a body into System and User sections.
// Looks for lines beginning with "System:" and "User:" (case-insensitive).
func parseSystemUser(body string) (system, user string) {
	body = strings.TrimSpace(body)
	lines := strings.Split(body, "\n")

	const (
		secNone = iota
		secSystem
		secUser
	)
	section := secNone
	var sysBuf, userBuf strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case lower == "system:" || strings.HasPrefix(lower, "system: "):
			section = secSystem
			rest := strings.TrimSpace(trimmed[len("system:"):])
			if rest != "" {
				sysBuf.WriteString(rest + "\n")
			}
		case lower == "user:" || strings.HasPrefix(lower, "user: "):
			section = secUser
			rest := strings.TrimSpace(trimmed[len("user:"):])
			if rest != "" {
				userBuf.WriteString(rest + "\n")
			}
		default:
			switch section {
			case secSystem:
				sysBuf.WriteString(line + "\n")
			case secUser:
				userBuf.WriteString(line + "\n")
			}
		}
	}
	return strings.TrimSpace(sysBuf.String()), strings.TrimSpace(userBuf.String())
}

// agentStarter returns an AgentStarterAdapter for starting agent sessions.
func (c *Commander) agentStarter() *AgentStarterAdapter {
	return NewAgentStarterAdapter(c.handlers)
}

// handleMemory dispatches /memory subcommands.
func (c *Commander) handleMemory(arg string) string {
	sub, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	switch strings.ToLower(sub) {
	case "git":
		return c.handleMemoryGit(strings.TrimSpace(rest))
	case "pull":
		return c.handleMemoryPull()
	case "push":
		return c.handleMemoryPush()
	case "keygen":
		return c.handleMemoryKeygen()
	case "pubkey":
		return c.handleMemoryPubkey()
	case "status", "":
		return c.handleMemoryStatus()
	default:
		return "unknown subcommand: /memory " + sub + "\nTry: /memory git <remote-url> · /memory pull · /memory push · /memory keygen · /memory pubkey · /memory status"
	}
}

// handleMemoryPull fetches from origin and rebases local commits on top.
func (c *Commander) handleMemoryPull() string {
	creds := tmpl.MemoryGitCredsFromEnv()
	remote, err := tmpl.PullMemory(c.cfg.MemoryDir, creds)
	if err != nil {
		return "error: " + err.Error()
	}
	return "memory pulled from " + remote
}

// handleMemoryPush commits any pending changes and pushes memory to origin.
func (c *Commander) handleMemoryPush() string {
	if _, err := os.Stat(filepath.Join(c.cfg.MemoryDir, ".git")); os.IsNotExist(err) {
		return "error: memory dir is not a git repo — run /memory git <remote-url> first"
	}
	creds := tmpl.MemoryGitCredsFromEnv()
	if err := tmpl.CommitAndPushMemory(c.cfg.MemoryDir, creds); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	remote := ""
	if out, err := exec.Command("git", "-C", c.cfg.MemoryDir, "remote", "get-url", "origin").Output(); err == nil {
		remote = strings.TrimSpace(string(out))
	}
	if remote == "" {
		return "memory pushed"
	}
	return "memory pushed to " + remote
}

// handleMemoryGit initialises or updates git-backed memory.
func (c *Commander) handleMemoryGit(remote string) string {
	if remote == "" {
		return "error: usage: /memory git <remote-url>"
	}
	creds := tmpl.MemoryGitCredsFromEnv()
	res, err := tmpl.InitMemoryGit(c.cfg.MemoryDir, remote, creds)
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	if res.Initialised {
		fmt.Fprintf(&b, "initialised git in %s\n", c.cfg.MemoryDir)
	}
	if res.RemoteChanged {
		if res.RemoteOld == "" {
			fmt.Fprintf(&b, "remote set: %s\n", res.RemoteNew)
		} else {
			fmt.Fprintf(&b, "remote updated: %s → %s\n", res.RemoteOld, res.RemoteNew)
		}
	} else {
		fmt.Fprintf(&b, "already configured: %s\n", res.RemoteNew)
	}
	b.WriteString("run /memory push to sync")
	return strings.TrimRight(b.String(), "\n")
}

// handleMemoryKeygen generates an ed25519 SSH key pair for memory git access.
// If a key already exists it is not overwritten; the public key is always printed.
func (c *Commander) handleMemoryKeygen() string {
	keyPath := filepath.Join(filepath.Dir(c.cfg.MemoryDir), "memory_key")
	pubPath := keyPath + ".pub"

	if _, err := os.Stat(keyPath); err == nil {
		// Key already exists — just print the public key
		pub, err := os.ReadFile(pubPath)
		if err != nil {
			return fmt.Sprintf("key exists at %s but cannot read public key: %v", keyPath, err)
		}
		return fmt.Sprintf("key already exists at %s\n\n%s\nAdd the public key above to GitHub/GitLab → Deploy Keys.", keyPath, strings.TrimSpace(string(pub)))
	}

	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-C", "agent-runner-memory")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("error: ssh-keygen failed: %v\n%s", err, stderr.String())
	}

	pub, err := os.ReadFile(pubPath)
	if err != nil {
		return fmt.Sprintf("key generated at %s but cannot read public key: %v", keyPath, err)
	}

	// Auto-save MEMORY_GIT_SSH_KEY
	if err := config.SetEnvLocal("MEMORY_GIT_SSH_KEY", keyPath); err != nil {
		return fmt.Sprintf("error: failed to save MEMORY_GIT_SSH_KEY: %v", err)
	}
	if err := os.Setenv("MEMORY_GIT_SSH_KEY", keyPath); err != nil {
		slog.Warn("ssh-keygen: failed to apply MEMORY_GIT_SSH_KEY to process", "error", err)
	}

	return fmt.Sprintf("generated SSH key: %s\nMEMORY_GIT_SSH_KEY saved.\n\nAdd this public key to GitHub/GitLab → Deploy Keys:\n\n%s", keyPath, strings.TrimSpace(string(pub)))
}

// handleMemoryPubkey prints the existing public key.
func (c *Commander) handleMemoryPubkey() string {
	keyPath := os.Getenv("MEMORY_GIT_SSH_KEY")
	if keyPath == "" {
		keyPath = filepath.Join(filepath.Dir(c.cfg.MemoryDir), "memory_key")
	}
	pubPath := keyPath + ".pub"
	pub, err := os.ReadFile(pubPath)
	if err != nil {
		return fmt.Sprintf("no public key found at %s — run /memory keygen first", pubPath)
	}
	return strings.TrimSpace(string(pub))
}

// handleMemoryStatus shows the state of the memory directory.
func (c *Commander) handleMemoryStatus() string {
	var b strings.Builder
	b.WriteString("**Memory**\n\n")
	fmt.Fprintf(&b, "**dir:** %s\n", c.cfg.MemoryDir)

	if _, err := os.Stat(filepath.Join(c.cfg.MemoryDir, ".git")); os.IsNotExist(err) {
		b.WriteString("**git:** not initialised (run /memory git <remote-url>)")
		return b.String()
	}
	b.WriteString("**git:** initialised\n")

	if out, err := exec.Command("git", "-C", c.cfg.MemoryDir, "remote", "get-url", "origin").Output(); err == nil {
		fmt.Fprintf(&b, "**remote:** %s\n", strings.TrimSpace(string(out)))
	} else {
		b.WriteString("**remote:** none\n")
	}

	if out, err := exec.Command("git", "-C", c.cfg.MemoryDir, "log", "-1", "--format=%s").Output(); err == nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			fmt.Fprintf(&b, "**last commit:** %s", msg)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// handleRepo dispatches /repo subcommands.
func (c *Commander) handleRepo(arg string) string {
	sub, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	switch strings.ToLower(sub) {
	case "add":
		return c.handleRepoAdd(strings.TrimSpace(rest))
	case "list":
		return c.handleRepoList()
	case "remove":
		return c.handleRepoRemove(strings.TrimSpace(rest))
	case "update":
		return c.handleRepoUpdate(strings.TrimSpace(rest))
	default:
		return "unknown subcommand: /repo " + sub +
			"\nTry: /repo add <url> · /repo list · /repo remove <name> · /repo update <name>"
	}
}

// handleRepoAdd clones a repo into REPO_CACHE_ROOT, strips the token from the
// stored remote, and configures a credential helper reading GIT_TOKEN from env.
func (c *Commander) handleRepoAdd(url string) string {
	if url == "" {
		return "error: usage: /repo add <url>"
	}
	if c.cfg.RepoCacheRoot == "" {
		return "error: REPO_CACHE_ROOT is not configured"
	}

	// Derive name: last path component, strip .git suffix.
	name := filepath.Base(strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git"))
	if name == "" || name == "." {
		return "error: cannot derive repo name from URL: " + url
	}

	cachePath := filepath.Join(c.cfg.RepoCacheRoot, name)
	if _, err := os.Stat(cachePath); err == nil {
		return name + " is already in cache (" + cachePath + ")"
	}

	// Clone — inject token into URL so credentials aren't stored.
	token := c.cfg.GitToken
	cloneURL := tmpl.InjectToken(url, token, "")
	cloneCmd := exec.Command("git", "clone", cloneURL, cachePath)
	var cloneStderr strings.Builder
	cloneCmd.Stderr = &cloneStderr
	if err := cloneCmd.Run(); err != nil {
		return fmt.Sprintf("error: git clone failed: %s", strings.TrimSpace(cloneStderr.String()))
	}

	// Strip token: set remote back to clean URL.
	setURL := exec.Command("git", "-C", cachePath, "remote", "set-url", "origin", url)
	var setURLStderr strings.Builder
	setURL.Stderr = &setURLStderr
	if err := setURL.Run(); err != nil {
		// Non-fatal — repo is cloned; just warn.
		return fmt.Sprintf("added %s (warning: could not strip token from remote: %v: %s)", name, err, strings.TrimSpace(setURLStderr.String()))
	}

	// Configure credential helper so future git ops pick up GIT_TOKEN from env.
	if token != "" {
		executor.ConfigureCredHelper(cachePath)
	}

	// Auto-append to AGENT_SHARED_REPOS so the repo is available to agents.
	shared := c.cfg.Agent.SharedRepos
	sharedNote := ""
	if !slices.Contains(shared, name) {
		shared = append(shared, name)
		newVal := strings.Join(shared, ",")
		if err := config.SetEnvLocal("AGENT_SHARED_REPOS", newVal); err == nil {
			if err := os.Setenv("AGENT_SHARED_REPOS", newVal); err != nil {
				slog.Warn("repo: failed to apply AGENT_SHARED_REPOS to process", "error", err)
			}
			c.cfg.Agent.SharedRepos = shared
			sharedNote = "; added to AGENT_SHARED_REPOS"
		}
	}

	return fmt.Sprintf("added %s from %s%s", name, url, sharedNote)
}

// handleRepoList walks REPO_CACHE_ROOT and prints each git repo with its
// remote and last commit summary.
func (c *Commander) handleRepoList() string {
	if c.cfg.RepoCacheRoot == "" {
		return "error: REPO_CACHE_ROOT is not configured"
	}
	entries, err := os.ReadDir(c.cfg.RepoCacheRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "no repos in cache (REPO_CACHE_ROOT does not exist yet)"
		}
		return "error: " + err.Error()
	}

	sharedSet := make(map[string]bool, len(c.cfg.Agent.SharedRepos))
	for _, r := range c.cfg.Agent.SharedRepos {
		sharedSet[r] = true
	}

	var b strings.Builder
	b.WriteString("**Cached repos**\n\n")
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(c.cfg.RepoCacheRoot, e.Name())
		if _, err := os.Stat(filepath.Join(p, ".git")); err != nil {
			continue // not a git repo
		}
		count++
		remote := ""
		if out, err := exec.Command("git", "-C", p, "remote", "get-url", "origin").Output(); err == nil {
			remote = strings.TrimSpace(string(out))
		}
		last := ""
		if out, err := exec.Command("git", "-C", p, "log", "-1", "--format=%h %s").Output(); err == nil {
			last = strings.TrimSpace(string(out))
		}
		fmt.Fprintf(&b, "**%s**", e.Name())
		if sharedSet[e.Name()] {
			b.WriteString(" ✓ shared")
		}
		if remote != "" {
			fmt.Fprintf(&b, " — %s", remote)
		}
		if last != "" {
			fmt.Fprintf(&b, "\n  %s", last)
		}
		b.WriteString("\n\n")
	}
	if count == 0 {
		return "no repos in cache"
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleRepoRemove deletes a cached repo by name, suggesting close matches on typo.
func (c *Commander) handleRepoRemove(name string) string {
	if name == "" {
		return "error: usage: /repo remove <name>"
	}
	if c.cfg.RepoCacheRoot == "" {
		return "error: REPO_CACHE_ROOT is not configured"
	}
	p := filepath.Join(c.cfg.RepoCacheRoot, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		msg := "error: repo not found: " + name
		if suggestion := executor.SuggestCachedRepo(c.cfg.RepoCacheRoot, name); suggestion != "" {
			msg += " (did you mean " + suggestion + "?)"
		}
		return msg
	}
	if err := os.RemoveAll(p); err != nil {
		return fmt.Sprintf("error: remove %s: %v", name, err)
	}

	// Drop from AGENT_SHARED_REPOS to keep it in sync with the cache.
	sharedNote := ""
	shared := c.cfg.Agent.SharedRepos
	filtered := shared[:0:0]
	for _, r := range shared {
		if r != name {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) < len(shared) {
		newVal := strings.Join(filtered, ",")
		if err := config.SetEnvLocal("AGENT_SHARED_REPOS", newVal); err == nil {
			if err := os.Setenv("AGENT_SHARED_REPOS", newVal); err != nil {
				slog.Warn("repo: failed to apply AGENT_SHARED_REPOS to process", "error", err)
			}
			c.cfg.Agent.SharedRepos = filtered
			sharedNote = "; removed from AGENT_SHARED_REPOS"
		}
	}

	return "removed " + name + sharedNote
}

// handleRepoUpdate fetches the latest from origin and resets the cached repo
// to origin/<default-branch>.
func (c *Commander) handleRepoUpdate(name string) string {
	if name == "" {
		return "error: usage: /repo update <name>"
	}
	if c.cfg.RepoCacheRoot == "" {
		return "error: REPO_CACHE_ROOT is not configured"
	}
	p := filepath.Join(c.cfg.RepoCacheRoot, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		msg := "error: repo not found: " + name
		if suggestion := executor.SuggestCachedRepo(c.cfg.RepoCacheRoot, name); suggestion != "" {
			msg += " (did you mean " + suggestion + "?)"
		}
		return msg
	}

	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = p
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}

	// Ensure credential helper is configured for repos not added via /repo add.
	// Always reconfigure (idempotent) rather than checking for a prior value —
	// a system/global credential.helper for the same host (e.g. macOS
	// osxkeychain) can satisfy a naive "is something already set" check while
	// actually being a stale credential for an unrelated project.
	if token := c.cfg.GitToken; token != "" {
		executor.ConfigureCredHelper(p)
	}

	if err := run("fetch", "origin"); err != nil {
		return fmt.Sprintf("error: git fetch: %v", err)
	}

	// Detect default branch; fall back to "main".
	branch := "main"
	if out, err := exec.Command("git", "-C", p, "symbolic-ref", "refs/remotes/origin/HEAD").Output(); err == nil {
		parts := strings.Split(strings.TrimSpace(string(out)), "/")
		if len(parts) > 0 {
			branch = parts[len(parts)-1]
		}
	}

	if err := run("reset", "--hard", "origin/"+branch); err != nil {
		return fmt.Sprintf("error: git reset: %v", err)
	}
	if err := run("clean", "-fdx"); err != nil {
		return fmt.Sprintf("error: git clean: %v", err)
	}

	return fmt.Sprintf("updated %s to origin/%s", name, branch)
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

**/status** — show runtime state (agent, queue, CLI, issues)
**/sessions** — list all active and recent sessions with IDs
**/stop** _\<session-id\>_ — request a running or queued session to stop
**/logs** _[n|id]_ — show last n agent logs (default 3); or /logs \<id-prefix\> for one session
**/config** — show current configuration and readiness

**/set** _KEY VALUE_ — set a config value (saved to .env.local, survives restart)
Examples: /set AGENT\_CLI claude · /set ANTHROPIC\_API\_KEY \<key\> · /set DEEPSEEK\_API\_KEY \<key\>

**/install-cli** _[cli]_ _[force]_ — install agent CLI (claude / codex / opencode); add **force** to reinstall even if already present

**/bootstrap** — create default agent.md and prompt.md
**/bootstrap force** — overwrite existing files

**/auth** _[cli]_ — start OAuth login flow via chat (claude or codex)
**/auth cancel** — stop an in-progress auth flow

**/set-agent** _\<content\>_ — overwrite agent.md
**/set-prompt** _\<content\>_ — overwrite prompt.md

**/memory git** _\<remote-url\>_ — init or update git remote for memory dir
  HTTPS: /set MEMORY\_GIT\_TOKEN \<token\> · /set MEMORY\_GIT\_USER \<username\> (optional, defaults to oauth2) · SSH: /memory keygen
**/memory pull** — pull latest memory from remote
**/memory push** — commit and push memory to remote
**/memory keygen** — generate SSH deploy key and print public key
**/memory pubkey** — print existing public key
**/memory status** — show memory dir, remote, and last commit

**/repo add** _\<url\>_ — clone repo into cache (uses GIT\_TOKEN if set)
**/repo list** — list cached repos with remote and last commit
**/repo remove** _\<name\>_ — delete cached repo (suggests closest match on typo)
**/repo update** _\<name\>_ — fetch latest from origin and reset to remote HEAD`

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
