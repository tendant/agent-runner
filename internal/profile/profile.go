// Package profile isolates spawned agent executors into self-contained
// configuration universes. A profile redirects each agent CLI's config
// directory — CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_CONFIG_HOME/XDG_DATA_HOME —
// into profiles/<name>/, so the MCP servers, skills, and credentials an
// agent sees are exactly what the profile provisions, and nothing else.
//
// Profiles isolate the tool surface, not the filesystem: a Bash-capable
// agent still runs as the host user. For enforcement, run one profile per
// container; the profile directory is the container contract.
//
// Layout:
//
//	profiles/<name>/
//	  profile.env   extra env (API keys, MAILDIRX_MCP_MODE, SEED_AUTH=true)
//	  mcp.json      MCP declaration (mcpsetup format)
//	  skills/       synced into the claude config dir's skills/
//	  claude/       ← CLAUDE_CONFIG_DIR
//	  codex/        ← CODEX_HOME
//	  xdg/          ← XDG_CONFIG_HOME  (opencode config)
//	  xdg-data/     ← XDG_DATA_HOME   (opencode auth)
package profile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-runner/agent-runner/internal/mcpsetup"
	"github.com/joho/godotenv"
)

// Root is the directory holding profiles, relative to the runner's cwd.
const Root = "profiles"

// seedAuthKey in profile.env opts the profile into copying the host's CLI
// credentials at provision time (for OAuth-based auth). It is consumed by
// provisioning and never exported to the agent environment.
const seedAuthKey = "SEED_AUTH"

// Dir returns the profile's directory.
func Dir(name string) string {
	return filepath.Join(Root, name)
}

// List returns the names of existing profiles.
func List() []string {
	entries, err := os.ReadDir(Root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Env returns the environment overlay for spawning an executor under the
// profile: the config-dir redirections plus profile.env contents. An empty
// name returns nil — the legacy inherit-everything behavior.
func Env(name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	dir, err := filepath.Abs(Dir(name))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("profile %q not found (run /profile provision %s or create %s)", name, name, dir)
	}

	env := []string{
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"),
		"CODEX_HOME=" + filepath.Join(dir, "codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "xdg-data"),
	}
	extra, err := readProfileEnv(dir)
	if err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(extra) {
		if k == seedAuthKey {
			continue
		}
		env = append(env, k+"="+extra[k])
	}
	return env, nil
}

// Provision creates the profile's config universe: directories, declared MCP
// servers, skills, and (opted in via SEED_AUTH=true) copies of the host's
// CLI credentials. Idempotent.
func Provision(name string) ([]mcpsetup.Result, error) {
	if name == "" {
		return nil, fmt.Errorf("profile name required")
	}
	dir, err := filepath.Abs(Dir(name))
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"claude", "codex", "xdg/opencode", "xdg-data"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			return nil, err
		}
	}

	extra, err := readProfileEnv(dir)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(extra[seedAuthKey], "true") {
		seedAuth(dir)
	}

	if err := syncSkills(dir); err != nil {
		return nil, err
	}

	cfg, err := mcpsetup.Load(filepath.Join(dir, "mcp.json"))
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

// seedAuth copies the host CLIs' credential files into the profile so
// OAuth-authenticated setups work without per-profile API keys. Best-effort:
// missing sources are skipped, existing copies are kept.
func seedAuth(dir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	pairs := [][2]string{
		{filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(dir, "claude", ".credentials.json")},
		{filepath.Join(home, ".codex", "auth.json"), filepath.Join(dir, "codex", "auth.json")},
		{filepath.Join(home, ".local", "share", "opencode", "auth.json"), filepath.Join(dir, "xdg-data", "opencode", "auth.json")},
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

// syncSkills copies profiles/<name>/skills/ into the claude config dir.
func syncSkills(dir string) error {
	src := filepath.Join(dir, "skills")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	dst := filepath.Join(dir, "claude", "skills")
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

func readProfileEnv(dir string) (map[string]string, error) {
	path := filepath.Join(dir, "profile.env")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	return godotenv.Read(path)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
