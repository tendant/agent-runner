// Package agenthome gives the runner's spawned agents a self-contained
// configuration universe: agent-home/ inside the runner directory. An
// agent-runner instance is one agent — its directory already defines the
// agent's identity (mcp.json, prompts, .env), and agent-home is where that
// identity materializes as CLI configuration.
//
// Isolation works by config-dir redirection: spawned CLIs get
// CLAUDE_CONFIG_DIR, CODEX_HOME, and XDG_CONFIG_HOME/XDG_DATA_HOME pointed
// into agent-home/, so the MCP servers, skills, and credentials they see are
// exactly what Provision put there — nothing inherited from the host user's
// own CLI configs. Enabled by AGENT_ISOLATED=true; off means the legacy
// inherit-everything behavior.
//
// This isolates the tool surface, not the filesystem: a shell-capable agent
// still runs as the host user. For enforcement, run the runner in a
// container; agent-home is part of the runner directory, so the container
// volume contract is unchanged.
package agenthome

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/agent-runner/agent-runner/internal/mcpsetup"
)

// Dir is the agent's config universe, relative to the runner directory.
const Dir = "agent-home"

// SkillsDir is the operator-owned skills source, synced into the claude
// config dir at provision time.
const SkillsDir = "skills"

// Env returns the environment overlay that spawns executors inside the
// agent home.
func Env() ([]string, error) {
	dir, err := filepath.Abs(Dir)
	if err != nil {
		return nil, err
	}
	return []string{
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"),
		"CODEX_HOME=" + filepath.Join(dir, "codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "xdg-data"),
		"PI_CODING_AGENT_DIR=" + filepath.Join(dir, "pi", "agent"),
		"PI_CODING_AGENT_SESSION_DIR=" + filepath.Join(dir, "pi", "sessions"),
	}, nil
}

// Provision materializes the agent home: directories, the MCP servers
// declared in mcp.json, skills from skills/, and copies of the host CLIs'
// credentials so OAuth-authenticated setups keep working. Idempotent.
func Provision() ([]mcpsetup.Result, error) {
	dir, err := filepath.Abs(Dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"claude", "codex", "xdg/opencode", "xdg-data", "pi/agent", "pi/sessions"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			return nil, err
		}
	}

	seedAuth(dir)

	if err := syncSkills(dir); err != nil {
		return nil, err
	}

	cfg, err := mcpsetup.Load(mcpsetup.DefaultPath)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	return mcpsetup.EnsureAll(cfg, mcpsetup.Paths{
		ClaudeConfigDir: filepath.Join(dir, "claude"),
		CodexConfig:     filepath.Join(dir, "codex", "config.toml"),
		OpencodeConfig:  filepath.Join(dir, "xdg", "opencode", "opencode.json"),
	}), nil
}

// seedAuth copies the host CLIs' credential and provider-config files into
// the agent home so agents can authenticate. For pi that includes
// models.json — custom providers (base URL + API key reference) live there,
// and without it an isolated pi agent can't reach any non-builtin endpoint.
// Best-effort: missing sources are skipped and existing copies kept (API
// keys via env work without any of this).
func seedAuth(dir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	pairs := [][2]string{
		{filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(dir, "claude", ".credentials.json")},
		{filepath.Join(home, ".codex", "auth.json"), filepath.Join(dir, "codex", "auth.json")},
		{filepath.Join(home, ".local", "share", "opencode", "auth.json"), filepath.Join(dir, "xdg-data", "opencode", "auth.json")},
		{filepath.Join(home, ".pi", "agent", "auth.json"), filepath.Join(dir, "pi", "agent", "auth.json")},
		{filepath.Join(home, ".pi", "agent", "models.json"), filepath.Join(dir, "pi", "agent", "models.json")},
	}
	for _, p := range pairs {
		src, dst := p[0], p[1]
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		_ = os.MkdirAll(filepath.Dir(dst), 0700)
		_ = os.WriteFile(dst, data, 0600)
	}
}

// syncSkills copies skills/ into each CLI's skills directory: claude's
// config dir and pi's agent dir (pi consumes Agent-Skills-standard skills
// instead of MCP servers).
func syncSkills(dir string) error {
	src := SkillsDir
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	for _, dst := range []string{
		filepath.Join(dir, "claude", "skills"),
		filepath.Join(dir, "pi", "agent", "skills"),
	} {
		if err := copyTree(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}
