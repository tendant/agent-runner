package runtime

import (
	"encoding/json"
	"errors"
	"github.com/agent-runner/agent-runner/internal/safefs"
	"io/fs"
	"os"
	"path/filepath"
)

// ClaudeSeed is where a sandboxed claude CLI's configuration comes from: the
// host's ~/.claude and ~/.claude.json, or agent-home/claude when the runner is
// isolated (AGENT_ISOLATED). Empty fields are skipped.
type ClaudeSeed struct {
	Dir       string // config dir to copy the allowlisted entries from
	StateFile string // .claude.json to take the allowlisted keys from
}

// claudeConfigEntries are the parts of a claude config dir a sandboxed run
// gets: settings, instructions, subagents, commands and skills. Never the
// credentials (.credentials.json; on macOS the login is in the Keychain
// anyway), and never other projects' transcripts, history, todos or shell
// snapshots, which the sandbox exists to hide.
var claudeConfigEntries = []string{"settings.json", "CLAUDE.md", "agents", "commands", "skills"}

// claudeStateKeys are the .claude.json keys copied: user-scope MCP servers and
// the onboarding flag. Not oauthAccount, not per-project history.
var claudeStateKeys = []string{"mcpServers", "hasCompletedOnboarding"}

// seedClaudeConfig refreshes rel, the claude config dir under the sandbox
// home, from seed before a run: the allowlisted entries are replaced with
// the seed's current copies (or removed when the seed no longer has them)
// and the allowlisted .claude.json keys are merged in. Everything else there,
// such as the CLI's own session transcripts that a task's next turn resumes,
// is kept. The home is the agent's to write, so everything goes through a
// root at it: a link it planted is never followed (internal/safefs).
func seedClaudeConfig(seed ClaudeSeed, home, rel string) error {
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := seedDirIn(seed.Dir, root, rel, claudeConfigEntries); err != nil {
		return err
	}
	if seed.StateFile == "" {
		return nil
	}
	src := map[string]json.RawMessage{}
	if b, err := os.ReadFile(seed.StateFile); err == nil {
		if err := json.Unmarshal(b, &src); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	statePath := filepath.Join(rel, ".claude.json")
	state := map[string]json.RawMessage{}
	if b, err := safefs.ReadIn(root, statePath, 4<<20); err == nil {
		_ = json.Unmarshal(b, &state) // a corrupt copy is rebuilt
	}
	for _, k := range claudeStateKeys {
		if v, ok := src[k]; ok {
			state[k] = v
		} else {
			delete(state, k)
		}
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return safefs.WriteIn(root, statePath, b, 0o600)
}

// seedDir refreshes the allowlisted entries of rel (under the sandbox home)
// from src: each is replaced with src's current copy, or removed when src no
// longer has it. Other entries (the CLI's own sessions and state) are kept.
// An empty src only makes sure rel exists.
func seedDir(src, home, rel string, entries []string) error {
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	return seedDirIn(src, root, rel, entries)
}

func seedDirIn(src string, root *os.Root, rel string, entries []string) error {
	if err := root.MkdirAll(rel, 0o700); err != nil {
		return err
	}
	if src == "" {
		return nil
	}
	for _, name := range entries {
		target := filepath.Join(rel, name)
		if err := root.RemoveAll(target); err != nil {
			return err
		}
		if err := copyEntry(filepath.Join(src, name), root, target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// codexConfigEntries and piConfigEntries are what a sandboxed codex or pi gets
// of its host (or agent-home) config: instructions, prompts and skills, and
// pi's settings. Never auth.json (logins), config.toml (it can select a
// ChatGPT login or other providers) or sessions. pi's models.json is written
// per run by the model channel (writePiModels).
var (
	codexConfigEntries = []string{"AGENTS.md", "prompts", "skills"}
	piConfigEntries    = []string{"AGENTS.md", "settings.json", "prompts", "skills"}
)

// copyEntry copies a host file or directory tree (src, trusted) to dst
// under root, following src's symlinks to their content so nothing in dst
// points back outside the sandbox. A directory reached twice through
// symlinks (a loop) is copied once.
func copyEntry(src string, root *os.Root, dst string) error {
	return copyResolved(src, root, dst, map[string]bool{})
}

func copyResolved(src string, root *os.Root, dst string, seen map[string]bool) error {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		b, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		return safefs.WriteIn(root, dst, b, 0o600)
	}
	if seen[real] {
		return nil
	}
	seen[real] = true
	if err := root.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		return err
	}
	for _, e := range ents {
		err := copyResolved(filepath.Join(real, e.Name()), root, filepath.Join(dst, e.Name()), seen)
		if err != nil && !errors.Is(err, fs.ErrNotExist) { // a dangling symlink is skipped
			return err
		}
	}
	return nil
}
