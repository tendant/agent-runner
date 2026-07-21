// Package clisetup owns agent-CLI lifecycle helpers shared by the HTTP
// bootstrap endpoint and the chat command layer: install/version checks,
// credential warnings, default prompt-file creation, and interactive CLI
// auth flows.
package clisetup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const authTimeout = 5 * time.Minute

var urlRe = regexp.MustCompile(`https?://\S+`)

// ansiRe matches ANSI/VT100 escape sequences (e.g. \x1b[0m, \x1b[1;32m).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
}

// RunAuthFlow runs the given CLI's auth command under the provided context
// (which may be cancelled by /auth cancel) with an overall 5-minute timeout.
// URLs found in the output are sent immediately so the user can open them.
// The function blocks until the CLI exits, the context is cancelled, or the timeout fires.
// Returns nil on success, non-nil on cancellation, timeout, or auth failure.
func RunAuthFlow(parent context.Context, cli string, send func(string)) error {
	ctx, cancel := context.WithTimeout(parent, authTimeout)
	defer cancel()

	var args []string
	switch cli {
	case "claude":
		args = []string{"auth", "login"}
	case "codex":
		// Device code grant (RFC 8628): CLI prints user_code + verification_uri;
		// no browser redirect or local callback server required.
		args = []string{"login", "--device-auth"}
	default:
		send(fmt.Sprintf("error: /auth only supports 'claude' and 'codex' (got %q)", cli))
		return fmt.Errorf("unsupported cli: %s", cli)
	}

	cmd := exec.CommandContext(ctx, cli, args...)

	outPR, outPW := io.Pipe()
	errPR, errPW := io.Pipe()
	cmd.Stdout = outPW
	cmd.Stderr = errPW

	if err := cmd.Start(); err != nil {
		send(fmt.Sprintf("error: failed to start %s auth: %v", cli, err))
		return err
	}

	scanDone := make(chan struct{}, 2)
	scanLines := func(r io.Reader) {
		defer func() { scanDone <- struct{}{} }()
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := stripANSI(scanner.Text())
			if line == "" {
				continue
			}
			slog.Debug("auth output", "cli", cli, "line", line)
			if m := urlRe.FindString(line); m != "" {
				send("Open this URL in your browser to authenticate " + cli + ":\n\n" + m)
			} else {
				send(line)
			}
		}
	}

	go scanLines(outPR)
	go scanLines(errPR)

	// cmd.Wait() blocks until the process exits AND all internal I/O goroutines
	// finish writing to outPW/errPW. Only after that is it safe to close the
	// write ends, which signals EOF to the scanner goroutines.
	waitErr := cmd.Wait()
	outPW.Close()
	errPW.Close()
	<-scanDone
	<-scanDone

	if waitErr != nil {
		switch ctx.Err() {
		case context.Canceled:
			send("auth cancelled")
			return fmt.Errorf("cancelled")
		case context.DeadlineExceeded:
			send("auth timed out (5 min)")
			return fmt.Errorf("timed out")
		default:
			send(fmt.Sprintf("%s auth failed: %v", cli, waitErr))
			return waitErr
		}
	}
	send(cli + " authenticated successfully.")
	return nil
}

const DefaultAgentMD = `# Agent

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
- {{MEMORY_DIR}}/lessons.md          — curated automatically after each session; you may append but should not rewrite it

To update your workflow instructions for future sessions, write to {{MEMORY_DIR}}/prompt.md.

Keep entries brief. Append — do not delete existing content unless correcting an error.

## Credentials

API keys and credentials (ANTHROPIC_API_KEY, OPENAI_API_KEY, AWS_*, etc.) are available directly
as environment variables — the server loads .env and .env.local at startup and all subprocesses
inherit them. No file loading is needed.

## When Done

Report what you did in 2-3 sentences. List files changed and key commands run.
`

const DefaultPromptMD = `# Task Workflow

1. Read the task carefully and identify what needs to change
2. Explore relevant files and understand the current state
3. Make the changes
4. Test if applicable
5. Commit with a descriptive message
6. Report what was done
`

// FileResult records what happened to one file during bootstrap.
type FileResult struct {
	Path    string
	Created bool // false = skipped (already exists)
}

// CreateBootstrapFiles writes default agent.md and prompt.md when they are
// missing. With force=true, existing files are overwritten. Returns one result
// per file and any write error encountered.
func CreateBootstrapFiles(systemPromptPath, promptFilePath string, force bool) ([]FileResult, error) {
	files := []struct {
		path    string
		content string
	}{
		{systemPromptPath, DefaultAgentMD},
		{promptFilePath, DefaultPromptMD},
	}

	var results []FileResult
	for _, f := range files {
		if !force {
			if _, err := os.Stat(f.path); err == nil {
				results = append(results, FileResult{Path: f.path, Created: false})
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(f.path), 0755); err != nil {
			return results, fmt.Errorf("failed to create dir for %s: %w", f.path, err)
		}
		if err := os.WriteFile(f.path, []byte(f.content), 0644); err != nil {
			return results, fmt.Errorf("failed to write %s: %w", f.path, err)
		}
		results = append(results, FileResult{Path: f.path, Created: true})
	}
	return results, nil
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

// CLIVersion returns the installed version string for the given CLI binary,
// or an empty string if it cannot be determined.
func CLIVersion(cli string) (string, error) {
	if cli == "" {
		cli = "opencode"
	}
	// Resolve to the full path so the version call uses the same binary
	// that CLIInstalled() found, regardless of PATH ordering.
	path, err := exec.LookPath(cli)
	if err != nil {
		slog.Warn("CLIVersion: binary not found", "cli", cli, "error", err)
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// Allow AppImage binaries (e.g. opencode on Linux) to run without FUSE
	// by extracting instead of mounting, and disable the Electron sandbox that
	// requires a setuid chrome-sandbox binary.
	cmd.Env = append(os.Environ(), "APPIMAGE_EXTRACT_AND_RUN=1", "ELECTRON_DISABLE_SANDBOX=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		// --version can fail for several reasons on Linux (no display, missing
		// shared libs, no FUSE). Try fallbacks before giving up:
		//  1. xvfb-run — if it's a display error and xvfb is installed
		//  2. AppImage extraction — read X-AppImage-Version from .desktop file;
		//     works without FUSE, a display, or any system libraries
		if strings.Contains(stderrStr, "DISPLAY") || strings.Contains(stderrStr, "X server") {
			if xvfb, xerr := exec.LookPath("xvfb-run"); xerr == nil {
				xvfbCmd := exec.CommandContext(ctx, xvfb, "-a", path, "--version")
				xvfbCmd.Env = append(os.Environ(), "APPIMAGE_EXTRACT_AND_RUN=1", "ELECTRON_DISABLE_SANDBOX=1")
				if xout, xerr := xvfbCmd.Output(); xerr == nil {
					return strings.TrimSpace(string(xout)), nil
				}
			}
		}
		if v := appImageVersion(path); v != "" {
			return v, nil
		}
		msg := stderrStr
		if msg == "" {
			msg = err.Error()
		}
		slog.Warn("CLIVersion: --version failed", "cli", cli, "path", path, "error", err, "stderr", msg)
		return "", fmt.Errorf("%s", msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// appImageVersion extracts an AppImage and reads the version from the embedded
// .desktop file (X-AppImage-Version field). Used as a fallback on headless
// Linux servers where the Electron binary can't open a display for --version.
func appImageVersion(path string) string {
	dir, err := os.MkdirTemp("", "appimage-version-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// --appimage-extract extracts the embedded squashfs without needing FUSE,
	// a display, or any system libraries. Do NOT set APPIMAGE_EXTRACT_AND_RUN=1
	// here — that flag causes the runtime to extract-and-run instead of just
	// extracting, which launches the binary and fails on headless servers.
	// Use full extraction only: filtered extraction (--appimage-extract *.desktop)
	// can be misinterpreted by some AppImage runtimes and cause the binary to run.
	cmd := exec.CommandContext(ctx, path, "--appimage-extract")
	cmd.Dir = dir
	if cmd.Run() != nil {
		return ""
	}

	// Every AppImage .desktop file contains X-AppImage-Version=<version>.
	entries, err := os.ReadDir(filepath.Join(dir, "squashfs-root"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".desktop") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "squashfs-root", e.Name()))
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "X-AppImage-Version="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// InstallCLI installs the given agent CLI backend.
// opencode is downloaded from GitHub releases; claude and codex use npm.
// Returns combined output and any error.
func InstallCLI(cli string) (string, error) {
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

// InstallHint returns a human-readable install hint for /config output.
func InstallHint(cli string) string {
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

// ResolveCLI mirrors executor.NewExecutor's own default resolution: anything
// other than "codex"/"opencode" (including "") runs the claude backend. Kept
// in sync with that switch so preflight checks reflect what will actually run.
func ResolveCLI(cli string) string {
	if cli == "codex" || cli == "opencode" {
		return cli
	}
	return "claude"
}

// PreflightAgentConfig verifies the configured agent CLI backend binary is
// installed before a session starts. Returns nil when the binary is present;
// otherwise a clear, actionable error. Call this before creating a session so
// a missing binary fails immediately instead of after workspace setup,
// planning, and iteration retries have already run.
//
// Missing credentials are intentionally NOT checked here — they're surfaced
// as non-fatal session warnings (see BootstrapWarnings) because some setups
// (local proxies, OAuth device flows, custom auth) don't rely on an API key
// env var, and a mis-detected key would otherwise block a working setup.
func PreflightAgentConfig(cli string) error {
	effectiveCLI := ResolveCLI(cli)
	if !CLIInstalled(effectiveCLI) {
		return fmt.Errorf("%s CLI is not installed — install it (%s) or run POST /bootstrap to auto-install", effectiveCLI, InstallHint(effectiveCLI))
	}
	return nil
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
