package chatcmd

import (
	"context"
	"fmt"
	"github.com/agent-runner/agent-runner/internal/gitsafe"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"strconv"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/agenthome"
	"github.com/agent-runner/agent-runner/internal/botcommon"
	"github.com/agent-runner/agent-runner/internal/clisetup"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/mcpsetup"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
	"github.com/agent-runner/agent-runner/internal/textutil"
)

const (
	cmdSetAgent  = "/set-agent"
	cmdSetPrompt = "/set-prompt"
	cmdMemory    = "/memory"
	cmdPrompt    = "/prompt"
	cmdSecret    = "/secret"
	cmdRepo      = "/repo"
)

// Runtime is the narrow surface of the API layer the Commander needs. It
// keeps the dependency one-directional: the api package implements Runtime
// (see Handlers) and constructs the Commander, never the other way round.
type Runtime interface {
	// BootstrapPaths returns the agent.md and prompt.md paths.
	BootstrapPaths() (systemPrompt, promptFile string)
	// AgentManager exposes session start/stop/inspection.
	AgentManager() *agent.Manager
	// RefreshRuntime rebuilds the executor and LLM clients after a config change.
	RefreshRuntime()
	// AgentStarter returns the session-starting facade used for
	// command-initiated agent sessions.
	AgentStarter() botcommon.AgentStarter
	// PromptPreview composes the prompt a session for message would get now.
	PromptPreview(message string) (string, error)
}

// Commander handles chat configuration commands. Zero LLM dependencies —
// safe to call before any model or provider is configured.
type Commander struct {
	cfg        *config.Config
	rt         Runtime
	authMu     sync.Mutex // guards against concurrent /auth flows
	authCancel func()     // non-nil while an auth flow is running
}

// NewCommander creates a Commander backed by the given config and runtime.
func NewCommander(cfg *config.Config, rt Runtime) *Commander {
	return &Commander{cfg: cfg, rt: rt}
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
	// Normalise: accept "config", "help", "status", ... without the slash,
	// but only as the whole message: "help me fix the login bug" or
	// "bootstrap the repo, force push" is a task for the agent.
	if !strings.HasPrefix(lower, "/") {
		for _, cmd := range bareCommands {
			if lower == cmd {
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
		"/help":          "usage: /help [secret|repo|memory]",
		"/config":        "usage: /config",
		"/status":        "usage: /status",
		"/sessions":      "usage: /sessions",
		"/set":           "usage: /set KEY VALUE  or  /set KEY=VALUE",
		"/bootstrap":     "usage: /bootstrap [force]",
		"/install-cli":   "usage: /install-cli [claude|codex|opencode|pi] [force]",
		"/install-mcp":   "usage: /install-mcp [server-name] — registers servers declared in mcp.json",
		cmdSetAgent:      "usage: " + cmdSetAgent + " <content>",
		cmdSetPrompt:     "usage: " + cmdSetPrompt + " <content>",
		"/auth":          "usage: /auth [claude|codex] — only available via chat",
		cmdMemory:        "usage: " + cmdMemory + " [status|log|undo <session-id>|git <url>|pull|push]",
		cmdSecret:        "usage: " + cmdSecret + " [list|set NAME VALUE [description]|remove NAME]",
		cmdPrompt:        "usage: " + cmdPrompt + " [show [agent|workflow]|check|preview [full] [message]]",
		cmdRepo:          repoUsage,
		"/stop":          "usage: /stop [session-id]",
		"/logs":          "usage: /logs [session-id-prefix]",
		"/update-prompt": "usage: /update-prompt <content>",
	}

	switch {
	case lower == "/help":
		return r(helpText)
	case strings.HasPrefix(lower, "/help "):
		switch strings.TrimSpace(lower[len("/help "):]) {
		case "secret", "secrets":
			return r(secretHelp)
		case "repo", "repos":
			return r(repoHelp)
		case "memory", "prompt":
			return r(memoryHelp)
		}
		return r("no help on that; try /help, /help secret, /help repo or /help memory")
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
	case lower == "/install-mcp" || strings.HasPrefix(lower, "/install-mcp "):
		arg := strings.TrimSpace(text[len("/install-mcp"):])
		return r(c.handleInstallMCP(arg))
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
	case lower == cmdSecret || strings.HasPrefix(lower, cmdSecret+" "):
		return r(c.handleSecret(strings.TrimSpace(text[len(cmdSecret):])))
	case lower == cmdPrompt || strings.HasPrefix(lower, cmdPrompt+" "):
		return r(c.handlePrompt(strings.TrimSpace(text[len(cmdPrompt):])))
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

// ValidateAuth resolves and validates the CLI name for an auth flow.
func (c *Commander) ValidateAuth(cli string) (string, error) {
	if cli == "" {
		cli = c.cfg.Agent.CLI
	}
	if cli == "" || cli == "pi" {
		return "", fmt.Errorf("pi authenticates via provider API keys (or models.json) — use /set <PROVIDER>_API_KEY <key> instead")
	}
	if cli == "opencode" {
		return "", fmt.Errorf("opencode authenticates via API keys — use /set <PROVIDER>_API_KEY <key> instead")
	}
	if cli != "claude" && cli != "codex" {
		return "", fmt.Errorf("/auth only supports 'claude' and 'codex' (got %q)", cli)
	}
	if !clisetup.CLIInstalled(cli) {
		return "", fmt.Errorf("%s is not installed — run /install-cli %s first", cli, cli)
	}
	return cli, nil
}

// RegisterAuthCancel locks the auth mutex and stores cancel for /auth cancel.
// Returns an error if another flow is already running.
func (c *Commander) RegisterAuthCancel(cancel context.CancelFunc) error {
	if !c.authMu.TryLock() {
		return fmt.Errorf("an auth flow is already in progress — use /auth cancel to stop it")
	}
	c.authCancel = cancel
	return nil
}

// ReleaseAuthCancel clears the stored cancel and unlocks the auth mutex.
func (c *Commander) ReleaseAuthCancel() {
	c.authCancel = nil
	c.authMu.Unlock()
}

// startAuthFlow starts an async auth flow for chat channels (Telegram, WeChat, stream).
func (c *Commander) startAuthFlow(cli string, send func(string)) (string, error) {
	cli, err := c.ValidateAuth(cli)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.RegisterAuthCancel(cancel); err != nil {
		cancel()
		return "", err
	}
	go func() {
		defer c.ReleaseAuthCancel()
		if err := clisetup.RunAuthFlow(ctx, cli, send); err != nil && ctx.Err() == nil {
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
	sessions := c.rt.AgentManager().ListActiveSessions()
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
	if last := c.rt.AgentManager().LastCompletedSession(); last != nil {
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
		cli = "pi"
	}
	if clisetup.CLIInstalled(cli) {
		fmt.Fprintf(&b, "**cli:** %s ✓\n", cli)
	} else {
		fmt.Fprintf(&b, "**cli:** %s ✗ (not installed — /install-cli)\n", cli)
	}

	// Sandbox
	fmt.Fprintf(&b, "**sandbox:** %s\n", clisetup.SandboxStatus(c.cfg.Agent.SandboxMode,
		c.cfg.Agent.SandboxBackend, c.cfg.Agent.SandboxRead))

	// Memory git state
	if _, err := os.Stat(filepath.Join(c.cfg.MemoryDir, ".git")); err == nil {
		if out, err := gitsafe.Command("-C", c.cfg.MemoryDir, "remote", "get-url", "origin").Output(); err == nil {
			fmt.Fprintf(&b, "**memory:** git ✓ → %s\n", gitsafe.RedactURL(strings.TrimSpace(string(out))))
		} else {
			b.WriteString("**memory:** git (no remote)\n")
		}
	}

	// Issues
	warnings := clisetup.BootstrapWarnings(cli, c.cfg.Agent.Provider)
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
	sessions := c.rt.AgentManager().ListSessions(20)
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
	logsRoot := c.cfg.LogsRoot
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
	sessions := c.rt.AgentManager().ListSessions(0)
	for _, s := range sessions {
		if s.ID == arg || strings.HasPrefix(s.ID, "agent-"+arg) || strings.HasPrefix(s.ID, arg) {
			if s.Status != agent.SessionStatusRunning && s.Status != agent.SessionStatusQueued {
				return fmt.Sprintf("session %s is not running (status: %s)", s.ID, s.Status)
			}
			if err := c.rt.AgentManager().StopSession(s.ID); err != nil {
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
		cli = "pi"
	}

	b.WriteString("**Configuration**\n\n")

	// Identity first: one runner directory is one agent.
	if dir, err := os.Getwd(); err == nil {
		fmt.Fprintf(&b, "**agent:** %s (%s)\n", filepath.Base(dir), dir)
	}
	if c.cfg.Agent.Isolated {
		state := "agent-home not provisioned yet"
		if _, err := os.Stat(filepath.Join(agenthome.Dir, "claude")); err == nil {
			state = "agent-home provisioned"
		}
		fmt.Fprintf(&b, "**isolated:** true (%s)\n", state)
	} else {
		b.WriteString("**isolated:** false (executors inherit host CLI configs — /set AGENT_ISOLATED true)\n")
	}
	if mcpCfg, err := mcpsetup.Load(mcpsetup.DefaultPath); err != nil {
		fmt.Fprintf(&b, "**mcp:** mcp.json invalid: %v\n", err)
	} else if mcpCfg == nil || len(mcpCfg.Servers) == 0 {
		b.WriteString("**mcp:** none declared (/bootstrap creates mcp.json.example)\n")
	} else {
		names := make([]string, 0, len(mcpCfg.Servers))
		for name := range mcpCfg.Servers {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "**mcp:** %s\n", strings.Join(names, ", "))
	}
	if v, err := clisetup.CLIVersion(cli); err != nil {
		fmt.Fprintf(&b, "**cli:** %s (installed, version check failed: %s)\n", cli, err)
	} else if v != "" {
		fmt.Fprintf(&b, "**cli:** %s %s (installed)\n", cli, v)
	} else if clisetup.CLIInstalled(cli) {
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
	systemPath, promptPath := c.rt.BootstrapPaths()
	fmt.Fprintf(&b, "**agent.md:** %s\n", fileState(systemPath))
	fmt.Fprintf(&b, "**prompt.md:** %s\n", fileState(promptPath))

	// Ready = no credential warnings.
	warnings := clisetup.BootstrapWarnings(cli, c.cfg.Agent.Provider)
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
	c.rt.RefreshRuntime()

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
		cli = "pi"
	}
	if !force && clisetup.CLIInstalled(cli) {
		if v, err := clisetup.CLIVersion(cli); err != nil {
			return fmt.Sprintf("ok %s already installed (version check failed: %s)", cli, err)
		} else if v != "" {
			return fmt.Sprintf("ok %s already installed (%s)", cli, v)
		}
		return fmt.Sprintf("ok %s already installed", cli)
	}
	out, err := clisetup.InstallCLI(cli)
	out = strings.TrimSpace(out)
	if err != nil {
		if out != "" {
			return fmt.Sprintf("error installing %s: %v\n%s", cli, err, out)
		}
		return fmt.Sprintf("error installing %s: %v", cli, err)
	}
	if v, err := clisetup.CLIVersion(cli); err != nil {
		return fmt.Sprintf("ok installed %s (version check failed: %s)\n%s", cli, err, out)
	} else if v != "" {
		return fmt.Sprintf("ok installed %s (%s)\n%s", cli, v, out)
	}
	return fmt.Sprintf("ok installed %s\n%s", cli, out)
}

// handleInstallMCP registers MCP servers declared in mcp.json into the
// active CLI's configuration. Only declared servers can be installed — chat
// input selects among them but never supplies a command.
func (c *Commander) handleInstallMCP(arg string) string {
	cfg, err := mcpsetup.Load(mcpsetup.DefaultPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if cfg == nil || len(cfg.Servers) == 0 {
		path, _ := filepath.Abs(mcpsetup.DefaultPath)
		return fmt.Sprintf("no MCP servers declared in %s\n"+
			"Run /bootstrap to create mcp.json.example next to it, rename to mcp.json, edit, then /install-mcp again.", path)
	}

	if arg != "" {
		srv, ok := cfg.Servers[arg]
		if !ok {
			names := make([]string, 0, len(cfg.Servers))
			for name := range cfg.Servers {
				names = append(names, name)
			}
			sort.Strings(names)
			return fmt.Sprintf("error: %q is not declared in mcp.json (declared: %s)", arg, strings.Join(names, ", "))
		}
		cfg = &mcpsetup.Config{Servers: map[string]mcpsetup.Server{arg: srv}}
	}

	// Isolated runners have exactly one config universe: agent-home. The
	// filtered declaration still comes from mcp.json, so chat can only ever
	// select among declared servers.
	if c.cfg.Agent.Isolated {
		results, err := agenthome.Provision()
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		var lines []string
		for _, res := range results {
			if arg == "" || res.Name == arg {
				lines = append(lines, res.String())
			}
		}
		return "ok (agent-home)\n" + strings.Join(lines, "\n")
	}

	cli := clisetup.ResolveCLI(c.cfg.Agent.CLI)
	var lines []string
	for _, res := range mcpsetup.Ensure(cli, cfg) {
		lines = append(lines, res.String())
	}
	return "ok\n" + strings.Join(lines, "\n")
}

// handleSetFile writes content to the given prompt file (agent.md or prompt.md).
// isSystem=true targets the system-prompt path, isSystem=false targets the prompt-file path.
func (c *Commander) handleSetFile(name, content string, isSystem bool) string {
	if content == "" {
		return fmt.Sprintf("error: usage: %s <content>", map[bool]string{true: cmdSetAgent, false: cmdSetPrompt}[isSystem])
	}
	systemPath, promptPath := c.rt.BootstrapPaths()
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
	systemPath, promptPath := c.rt.BootstrapPaths()
	results, err := clisetup.CreateBootstrapFiles(systemPath, promptPath, force)
	if err != nil {
		return "error: " + err.Error()
	}

	var b strings.Builder
	for _, r := range results {
		if r.Created {
			fmt.Fprintf(&b, "created %s\n", r.Path)
		} else {
			fmt.Fprintf(&b, "skipped %s (already exists)\n", r.Path)
		}
	}

	// A new agent directory also needs its tool declaration and isolation
	// switch; templates only — never overwrite operator files.
	if _, err := os.Stat("mcp.json"); os.IsNotExist(err) {
		if err := os.WriteFile("mcp.json.example", []byte(mcpsetup.Example), 0644); err == nil {
			b.WriteString("created mcp.json.example (rename to mcp.json and edit — the agent's MCP servers)\n")
		}
	}
	if _, err := os.Stat(".env"); os.IsNotExist(err) {
		envTemplate := "# This directory is one agent. Executors run isolated in agent-home/\n" +
			"# with only the MCP servers declared in mcp.json.\n" +
			"AGENT_ISOLATED=true\n"
		if err := os.WriteFile(".env", []byte(envTemplate), 0644); err == nil {
			b.WriteString("created .env (AGENT_ISOLATED=true — /set AGENT_ISOLATED false to disable)\n")
		}
	}

	cli := c.cfg.Agent.CLI
	if cli == "" {
		cli = "pi"
	}
	warnings := clisetup.BootstrapWarnings(cli, c.cfg.Agent.Provider)
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
		_, promptPath := c.rt.BootstrapPaths()
		if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err == nil {
			_ = os.WriteFile(promptPath, []byte(systemContent+"\n"), 0644)
		}
	} else if userContent == "" {
		// No sections — treat the whole body as prompt content.
		if body != "" {
			_, promptPath := c.rt.BootstrapPaths()
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
	sid, err := c.rt.AgentStarter().StartAgent(userContent, "commander", "")
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
	case "log":
		return c.handleMemoryLog()
	case "undo":
		return c.handleMemoryUndo(strings.TrimSpace(rest))
	default:
		return "unknown subcommand: /memory " + sub + "\nTry: /memory git <remote-url> · /memory pull · /memory push · /memory log · /memory undo <session-id> · /memory keygen · /memory pubkey · /memory status"
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
	if out, err := gitsafe.Command("-C", c.cfg.MemoryDir, "remote", "get-url", "origin").Output(); err == nil {
		remote = strings.TrimSpace(string(out))
	}
	if remote == "" {
		return "memory pushed"
	}
	return "memory pushed to " + gitsafe.RedactURL(remote)
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
			fmt.Fprintf(&b, "remote set: %s\n", gitsafe.RedactURL(res.RemoteNew))
		} else {
			fmt.Fprintf(&b, "remote updated: %s → %s\n", gitsafe.RedactURL(res.RemoteOld), gitsafe.RedactURL(res.RemoteNew))
		}
	} else {
		fmt.Fprintf(&b, "already configured: %s\n", gitsafe.RedactURL(res.RemoteNew))
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

	if out, err := gitsafe.Command("-C", c.cfg.MemoryDir, "remote", "get-url", "origin").Output(); err == nil {
		fmt.Fprintf(&b, "**remote:** %s\n", gitsafe.RedactURL(strings.TrimSpace(string(out))))
	} else {
		b.WriteString("**remote:** none\n")
	}

	if out, err := gitsafe.Command("-C", c.cfg.MemoryDir, "log", "-1", "--format=%s").Output(); err == nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			fmt.Fprintf(&b, "**last commit:** %s", msg)
		}
	}

	return strings.TrimRight(b.String(), "\n")
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
	if upper == "API_KEY" {
		return true
	}
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

const helpText = `**Agent Runner** — send a message to give the agent a task; commands start with /.

**Getting started**
1. A credential the agent needs: /secret set GITEE\_TOKEN \<token\> Gitee API
2. A repo for every session: /repo add https://\<user\>@gitee.com/org/repo.git secret=GITEE\_TOKEN
3. Then just describe the task in a message. The agent commits; agent-runner pushes.
More: **/help secret** · **/help repo** · **/help memory**

**Sessions**
**/status** — what is running and queued, and any problems
**/sessions** — active and recent sessions with their ids
**/stop** _[session-id]_ — stop a session (the current one without an id)
**/logs** _[n | id-prefix]_ — the last n session logs (default 3), or one session's

**Repos and secrets**
**/repo add** _\<url\> [secret=NAME] [branch=B] [push=false]_ — a repo every session gets · **/repo list** · **/repo set** _\<name\> key=value_ · **/repo remove** _\<name\>_
**/secret set** _NAME VALUE [description]_ — a credential for the agent's tasks · **/secret list** · **/secret remove** _NAME_

**Memory and instructions**
**/memory log** — recent memory changes and the session behind each · **/memory undo** _\<session-id\>_ — take one session's notes back
**/memory status** · **/memory pull** · **/memory push** · **/memory git** _\<remote-url\>_ · **/memory keygen** · **/memory pubkey**
**/prompt show** _[agent|workflow]_ · **/prompt check** — out-of-date lines · **/prompt preview** _[full] [message]_ — what a session would get
**/set-agent** _\<content\>_ — replace agent.md · **/set-prompt** _\<content\>_ — replace prompt.md

**Setup** (agent-runner itself)
**/config** — settings and readiness
**/set** _KEY VALUE_ — an agent-runner setting, saved to .env.local (e.g. /set AGENT\_CLI claude · /set ANTHROPIC\_API\_KEY \<key\>); credentials for the agent's tasks go in /secret instead
**/install-cli** _[claude|codex|opencode|pi] [force]_ · **/install-mcp** _[name]_ · **/auth** _[claude|codex]_ · **/auth cancel**
**/bootstrap** _[force]_ — create the default agent.md and prompt.md (force overwrites)`

// repoHelp is /repo help (and /help repo).
const repoHelp = `**Repos** are the git repositories every session works on. agent-runner fetches each one
before a session and checks it out in the agent's working directory; the agent commits, and
agent-runner pushes its commits when the session ends.

**Add one** — the URL, then options:
  /repo add https://lei@gitee.com/org/oam-server.git secret=GITEE_TOKEN
  /repo add https://github.com/org/docs.git push=false
  /repo add git@gitee.com:org/oam-ops.git secret=GITEE_SSH_KEY
- secret=NAME — the /secret it logs in with (a token for https://, a private key for git@ / ssh://);
  none for a public https:// repo
- the user name goes in the URL when the host needs it (Gitee: https://<user>@gitee.com/...);
  otherwise oauth2 is used
- branch=B — the branch sessions start on (default: the repo's default branch)
- push=false — read-only: never pushed
- name=N — the folder name (default: from the URL)

Adding fetches the repo right away, so a wrong URL, branch or token shows up in the reply.

**Change one:** /repo set oam-server branch=develop · /repo set oam-server secret=NEW_TOKEN
**See them:** /repo list · **Remove one:** /repo remove oam-server

Never put a password or token in the URL: give it with /secret.`

// memoryHelp is /help memory.
const memoryHelp = `**Memory** is what the agent keeps between sessions: Markdown files (preferences, decisions,
lessons, topic notes) shared by everyone who talks to it. Each session works on a copy; what it
saves is merged back when it ends, as its own commit when memory is a git repo.

**Recent changes:** /memory log — each with the session that made it
**Take one back:** /memory undo <session-id>
**Where it is:** /memory status
**Sync with a git remote:** /memory git <remote-url> once (then /set MEMORY_GIT_TOKEN <token>, or
/memory keygen for an SSH deploy key); /memory pull and /memory push by hand, pushes also happen
after every session

**The agent's instructions** (agent.md, prompt.md) are yours: agents cannot change them.
/prompt show · /prompt check · /prompt preview · /set-agent <content> · /set-prompt <content>`

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
