package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

const (
	cmdSetAgent  = "/set-agent"
	cmdSetPrompt = "/set-prompt"
	cmdMemory    = "/memory"
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
var bareCommands = []string{"help", "config", "status", "bootstrap"}

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
	case lower == "/status":
		return c.handleStatus(), true
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
	case lower == cmdMemory || strings.HasPrefix(lower, cmdMemory+" "):
		arg := strings.TrimSpace(text[len(cmdMemory):])
		return c.handleMemory(arg), true
	}
	return "", false
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
		runCLIAuthFlowCtx(ctx, cli, send) //nolint:errcheck
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
			if len(preview) > 60 {
				preview = preview[:60] + "..."
			}
			fmt.Fprintf(&b, "**agent:** %s — %q [iter %d/%d]\n",
				s.Status, preview, s.CurrentIteration, s.MaxIterations)
		}
	}
	fmt.Fprintf(&b, "**queued:** %d\n", len(waiting))

	// Last completed session
	if last := c.handlers.agentManager.LastCompletedSession(); last != nil {
		ago := time.Since(*last.CompletedAt).Round(time.Second)
		preview := last.Message
		if len(preview) > 50 {
			preview = preview[:50] + "..."
		}
		if last.Status == agent.SessionStatusCompleted {
			fmt.Fprintf(&b, "**last:** completed ✓ %s ago — %q\n", ago, preview)
		} else {
			errMsg := last.Error
			if len(errMsg) > 60 {
				errMsg = errMsg[:60] + "..."
			}
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
	warnings := bootstrapWarnings(cli, c.cfg.Agent.Provider)
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

	// Repo git credentials (shown when set; token value hidden).
	if os.Getenv("GIT_TOKEN") != "" {
		b.WriteString("**GIT_TOKEN:** set\n")
	}
	if v := os.Getenv("GIT_SSH_KEY"); v != "" {
		fmt.Fprintf(&b, "**GIT_SSH_KEY:** %s\n", v)
	}

	// Memory git credentials (shown when set; values hidden).
	if os.Getenv("MEMORY_GIT_TOKEN") != "" {
		b.WriteString("**MEMORY_GIT_TOKEN:** set\n")
	}
	if v := os.Getenv("MEMORY_GIT_SSH_KEY"); v != "" {
		fmt.Fprintf(&b, "**MEMORY_GIT_SSH_KEY:** %s\n", v)
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
	creds := tmpl.MemoryGitCreds{
		Token:  os.Getenv("MEMORY_GIT_TOKEN"),
		SSHKey: os.Getenv("MEMORY_GIT_SSH_KEY"),
	}
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
	creds := tmpl.MemoryGitCreds{
		Token:  os.Getenv("MEMORY_GIT_TOKEN"),
		SSHKey: os.Getenv("MEMORY_GIT_SSH_KEY"),
	}
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
	creds := tmpl.MemoryGitCreds{
		Token:  os.Getenv("MEMORY_GIT_TOKEN"),
		SSHKey: os.Getenv("MEMORY_GIT_SSH_KEY"),
	}
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
	if res.Pushed {
		b.WriteString("pushed")
	} else if res.PushErr != nil {
		fmt.Fprintf(&b, "push failed: %v\nIf the new remote has unrelated history, force-push manually:\n  git -C <memory-dir> push origin HEAD --force", res.PushErr)
	}
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
	os.Setenv("MEMORY_GIT_SSH_KEY", keyPath) //nolint:errcheck

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
**/config** — show current configuration and readiness

**/set** _KEY VALUE_ — set a config value (saved to .env.local, survives restart)
Examples: /set AGENT\_CLI claude · /set ANTHROPIC\_API\_KEY \<key\> · /set DEEPSEEK\_API\_KEY \<key\>

**/install-cli** _[cli]_ — install agent CLI (claude / codex / opencode)

**/bootstrap** — create default agent.md and prompt.md
**/bootstrap force** — overwrite existing files

**/auth** _[cli]_ — start OAuth login flow via chat (claude or codex)
**/auth cancel** — stop an in-progress auth flow

**/set-agent** _\<content\>_ — overwrite agent.md
**/set-prompt** _\<content\>_ — overwrite prompt.md

**/memory git** _\<remote-url\>_ — init or update git remote for memory dir
  HTTPS: /set MEMORY\_GIT\_TOKEN \<token\> first · SSH: /memory keygen
**/memory pull** — pull latest memory from remote
**/memory push** — commit and push memory to remote
**/memory keygen** — generate SSH deploy key and print public key
**/memory pubkey** — print existing public key
**/memory status** — show memory dir, remote, and last commit`

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
