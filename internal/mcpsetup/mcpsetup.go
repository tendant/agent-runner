// Package mcpsetup provisions MCP servers into the agent CLIs' own
// configuration, from an operator-owned declaration file (mcp.json).
//
// Registration is reconciliation: the declaration states which servers each
// executor should have, and Ensure converges the CLI configs to match —
// idempotently, using each tool's native mechanism (the claude CLI for
// claude, config-file merges for codex and opencode).
//
// Security: an MCP registration configures arbitrary command execution, so
// declarations come only from the operator's mcp.json. Chat commands may
// trigger installation of declared servers but can never supply a command.
package mcpsetup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Server declares one MCP server an executor CLI should have registered.
type Server struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Config is the operator-owned declaration, loaded from mcp.json.
type Config struct {
	Servers map[string]Server `json:"servers"`
}

// DefaultPath is where the declaration lives, relative to the agent-runner
// working directory.
const DefaultPath = "mcp.json"

// Load reads a declaration file. A missing file returns (nil, nil) — no
// declaration is a normal state. "~" in commands and env values expands to
// the home directory so declarations stay portable.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	for name, srv := range cfg.Servers {
		if strings.TrimSpace(srv.Command) == "" {
			return nil, fmt.Errorf("%s: server %q has no command", path, name)
		}
		srv.Command = expandHome(srv.Command)
		for k, v := range srv.Env {
			srv.Env[k] = expandHome(v)
		}
		cfg.Servers[name] = srv
	}
	return &cfg, nil
}

// Result reports what Ensure did for one server.
type Result struct {
	CLI    string
	Name   string
	Action string // "registered", "updated", "present", or "error"
	Err    error
}

func (r Result) String() string {
	if r.Err != nil {
		return fmt.Sprintf("%s/%s: error: %v", r.CLI, r.Name, r.Err)
	}
	return fmt.Sprintf("%s/%s: %s", r.CLI, r.Name, r.Action)
}

// Ensure converges the given CLI's configuration to include every declared
// server. Unknown CLIs report an error result per server.
func Ensure(cli string, cfg *Config) []Result {
	if cfg == nil || len(cfg.Servers) == 0 {
		return nil
	}

	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	var results []Result
	for _, name := range names {
		srv := cfg.Servers[name]
		var action string
		var err error
		switch cli {
		case "claude":
			action, err = ensureClaude(name, srv)
		case "codex":
			action, err = ensureCodexAt(codexConfigPath(), name, srv)
		case "opencode":
			action, err = ensureOpencodeAt(opencodeConfigPath(), name, srv)
		default:
			err = fmt.Errorf("unknown CLI %q", cli)
		}
		if err != nil {
			results = append(results, Result{CLI: cli, Name: name, Action: "error", Err: err})
			continue
		}
		results = append(results, Result{CLI: cli, Name: name, Action: action})
	}
	return results
}

// --- claude: delegate to the claude CLI, which owns ~/.claude.json ---

// runCommand is swapped in tests. extraEnv entries overlay the inherited
// environment (used to redirect CLAUDE_CONFIG_DIR into a profile).
var runCommand = func(extraEnv []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

func ensureClaude(name string, srv Server) (string, error) {
	return ensureClaudeIn("", name, srv)
}

// ensureClaudeIn registers via the claude CLI, optionally redirected into a
// profile's config dir. The CLI owns its config file in both cases.
func ensureClaudeIn(configDir, name string, srv Server) (string, error) {
	var env []string
	if configDir != "" {
		env = []string{"CLAUDE_CONFIG_DIR=" + configDir}
	}
	// `claude mcp get <name>` exits non-zero when the server is unknown.
	if _, err := runCommand(env, "claude", "mcp", "get", name); err == nil {
		return "present", nil
	}
	out, err := runCommand(env, "claude", claudeAddArgs(name, srv)...)
	if err != nil {
		return "", fmt.Errorf("claude mcp add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return "registered", nil
}

// Paths targets Ensure at a profile's config universe instead of the host
// defaults. Zero fields fall back to the host locations.
type Paths struct {
	ClaudeConfigDir string
	CodexConfig     string
	OpencodeConfig  string
}

// EnsureAll converges every supported client on the declaration, targeted at
// the given paths — used when provisioning an isolated executor profile.
func EnsureAll(cfg *Config, paths Paths) []Result {
	if cfg == nil || len(cfg.Servers) == 0 {
		return nil
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	codexPath := paths.CodexConfig
	if codexPath == "" {
		codexPath = codexConfigPath()
	}
	opencodePath := paths.OpencodeConfig
	if opencodePath == "" {
		opencodePath = opencodeConfigPath()
	}

	var results []Result
	for _, name := range names {
		srv := cfg.Servers[name]
		for _, entry := range []struct {
			cli string
			fn  func() (string, error)
		}{
			{"claude", func() (string, error) { return ensureClaudeIn(paths.ClaudeConfigDir, name, srv) }},
			{"codex", func() (string, error) { return ensureCodexAt(codexPath, name, srv) }},
			{"opencode", func() (string, error) { return ensureOpencodeAt(opencodePath, name, srv) }},
		} {
			action, err := entry.fn()
			if err != nil {
				results = append(results, Result{CLI: entry.cli, Name: name, Action: "error", Err: err})
				continue
			}
			results = append(results, Result{CLI: entry.cli, Name: name, Action: action})
		}
	}
	return results
}

// claudeAddArgs builds the `claude mcp add` invocation for a declaration.
func claudeAddArgs(name string, srv Server) []string {
	args := []string{"mcp", "add", "--scope", "user", name}
	for _, k := range sortedKeys(srv.Env) {
		args = append(args, "--env", k+"="+srv.Env[k])
	}
	args = append(args, "--", srv.Command)
	args = append(args, srv.Args...)
	return args
}

// --- codex: append a [mcp_servers.<name>] table to config.toml ---

func codexConfigPath() string {
	return filepath.Join(homeDir(), ".codex", "config.toml")
}

func ensureCodexAt(path, name string, srv Server) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	header := "[mcp_servers." + name + "]"
	if strings.Contains(string(data), header) {
		// Present in some form; codex owns richer edits (`codex mcp`).
		return "present", nil
	}

	var b strings.Builder
	b.WriteString("\n# Added by agent-runner mcpsetup " + time.Now().Format("2006-01-02") + "\n")
	b.WriteString(header + "\n")
	b.WriteString(fmt.Sprintf("command = %q\n", srv.Command))
	if len(srv.Args) > 0 {
		quoted := make([]string, len(srv.Args))
		for i, a := range srv.Args {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		b.WriteString("args = [" + strings.Join(quoted, ", ") + "]\n")
	}
	if len(srv.Env) > 0 {
		pairs := make([]string, 0, len(srv.Env))
		for _, k := range sortedKeys(srv.Env) {
			pairs = append(pairs, fmt.Sprintf("%s = %q", k, srv.Env[k]))
		}
		b.WriteString("env = { " + strings.Join(pairs, ", ") + " }\n")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return "", err
	}
	return "registered", nil
}

// --- opencode: merge into the "mcp" object of opencode.json ---

func opencodeConfigPath() string {
	return filepath.Join(homeDir(), ".config", "opencode", "opencode.json")
}

func ensureOpencodeAt(path, name string, srv Server) (string, error) {
	cfg := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	command := append([]string{srv.Command}, srv.Args...)
	desired := map[string]any{
		"type":    "local",
		"command": toAnySlice(command),
		"enabled": true,
	}
	if len(srv.Env) > 0 {
		env := map[string]any{}
		for k, v := range srv.Env {
			env[k] = v
		}
		desired["environment"] = env
	}

	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	if existing, ok := mcp[name]; ok {
		// Round-trip through JSON so numbers/slices compare canonically.
		if reflect.DeepEqual(canonical(existing), canonical(desired)) {
			return "present", nil
		}
		mcp[name] = desired
		cfg["mcp"] = mcp
		if err := writeJSON(path, cfg); err != nil {
			return "", err
		}
		return "updated", nil
	}

	mcp[name] = desired
	cfg["mcp"] = mcp
	if err := writeJSON(path, cfg); err != nil {
		return "", err
	}
	return "registered", nil
}

// --- helpers ---

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func canonical(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return out
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

func expandHome(s string) string {
	if s == "~" || strings.HasPrefix(s, "~/") {
		return filepath.Join(homeDir(), strings.TrimPrefix(strings.TrimPrefix(s, "~"), "/"))
	}
	return s
}
