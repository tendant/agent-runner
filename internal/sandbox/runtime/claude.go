package runtime

import (
	"encoding/json"
	"errors"
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

// seedClaudeConfig refreshes dst, the sandbox home's claude config dir, from
// seed before a run: the allowlisted entries are replaced with the seed's
// current copies (or removed when the seed no longer has them) and the
// allowlisted .claude.json keys are merged in. Everything else in dst, such as
// the CLI's own session transcripts that a task's next turn resumes, is kept.
func seedClaudeConfig(seed ClaudeSeed, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if seed.Dir != "" {
		for _, name := range claudeConfigEntries {
			target := filepath.Join(dst, name)
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := copyEntry(filepath.Join(seed.Dir, name), target); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
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
	statePath := filepath.Join(dst, ".claude.json")
	state := map[string]json.RawMessage{}
	if b, err := os.ReadFile(statePath); err == nil {
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
	return os.WriteFile(statePath, b, 0o600)
}

// copyEntry copies a file or directory tree, following symlinks to their
// content so nothing in dst points back outside the sandbox. A directory
// reached twice through symlinks (a loop) is copied once.
func copyEntry(src, dst string) error {
	return copyResolved(src, dst, map[string]bool{})
}

func copyResolved(src, dst string, seen map[string]bool) error {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(real, dst)
	}
	if seen[real] {
		return nil
	}
	seen[real] = true
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		return err
	}
	for _, e := range ents {
		err := copyResolved(filepath.Join(real, e.Name()), filepath.Join(dst, e.Name()), seen)
		if err != nil && !errors.Is(err, fs.ErrNotExist) { // a dangling symlink is skipped
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
