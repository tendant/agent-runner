package clisetup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Every default agent.md and prompt.md agent-runner has shipped, as
// sha256(normalizePrompt(text)). A file still matching one was never edited,
// so it is safe to replace with the current default (UpgradeDefaultPrompts).
var (
	pastAgentMDs = map[string]bool{
		"079e48a618626c448e7a22eff788954fe9ff107b54e2466fe920693cfb7d948b": true,
		"09882dc12b964f819f1dbd4fd8a61a4ac917c966979f21601bbc00d463f1561a": true,
		"0c9e251bcf4147a28c274ce8700e801f4fbf155d3741bf55011dffa9b845f425": true,
		"401d444b585aa8798b275fb42eb89d1d5abcbebb4b24c0396dad7b485883a13c": true,
		"40b8453da8a68843498511202dc21adc1fd5cad9b430b627486404b7c3aeb8bb": true,
		"4317a546d1bce9999d07a7096df1187876f290bf8a0d93837d887389c0a29e16": true,
		"7fa5c96bbbb1bdbf12b0882710b0d816b0703b0153a0e30ed03b5644bfc99511": true,
		"8930a4e8a25926437a9121604ef87a4ec7e714130edda47b1d387c25988ce196": true,
		"9105c1d8cc5a558b0991f75c4da936e2089806e79c40734590bf7914da1dd611": true,
		"a1e43230592067f8459c6d8b6b33aa43604b0103bfb55bb7328e58487749cea0": true,
		"ac5bcbe128561e226ac65f30fa45556d9e4862755ef405040e4bad7a0b528f3f": true,
		"b9fdaa941ed16b2b5e7f3a2526ccabeeca0cc7b9f0405b932ac54d46abecd20a": true,
		"be85e796d1bec4409ae5584a2f5f4e21996d4d09b654abbd7afc4ca395008397": true,
		"d9ec913d61f7b387d8e47b169f6e655cfd77db69bf4bcc69cd47a3bb1d365e0f": true,
	}
	pastPromptMDs = map[string]bool{
		"0be2b2d29b3b850c02b8931d912180481ca43ed967dfea8ef7d32fd5b2988af8": true,
		"0c4f11e0f11739a072546f8d792e33657448dfe720d9bf2f2459812a2023061f": true,
		"c234ae51a34e9566ff424743beb072551e861a52c7131c60d7d729c8eb88af0b": true,
	}
)

// normalizePrompt ignores what an editor or a copy may change without
// anyone meaning to: line endings and trailing spaces.
func normalizePrompt(s string) string {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

func promptHash(s string) string {
	h := sha256.Sum256([]byte(normalizePrompt(s)))
	return hex.EncodeToString(h[:])
}

// UpgradeDefaultPrompts replaces agent.md and prompt.md with the current
// defaults where they are an earlier default nobody edited. Edited files are
// left alone (PromptCheck says what in them is out of date). Returns the
// paths it replaced.
func UpgradeDefaultPrompts(systemPromptPath, promptFilePath string) ([]string, error) {
	var done []string
	for _, f := range []struct {
		path, current string
		past          map[string]bool
	}{
		{systemPromptPath, DefaultAgentMD, pastAgentMDs},
		{promptFilePath, DefaultPromptMD, pastPromptMDs},
	} {
		b, err := os.ReadFile(f.path)
		if err != nil {
			continue // missing: /bootstrap creates it
		}
		h := promptHash(string(b))
		if !f.past[h] || h == promptHash(f.current) {
			continue
		}
		if err := writeAtomic(f.path, f.current); err != nil {
			return done, fmt.Errorf("upgrade %s: %w", f.path, err)
		}
		done = append(done, f.path)
	}
	return done, nil
}

func writeAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// promptChecks are phrases of earlier defaults (or common edits) that now
// duplicate or contradict the runtime layer the runner adds to every prompt.
var promptChecks = []struct{ phrase, problem string }{
	{"are available directly\nas environment variables", "says API keys are in the environment; the runner's Credentials section says what is"},
	{"and push to origin", "tells the agent to push; whether it pushes depends on the sandbox, and the runner says so"},
	{"git pull --rebase", "describes pushing and rebasing, which the runner's Git section covers per session"},
	{"Your memory directory is {{MEMORY_DIR}}", "describes memory, which the runner's Memory section covers per session"},
	{"write to {{MEMORY_DIR}}/prompt.md", "invites the agent to edit its own instructions; it may not"},
	{"{{API_KEY}}", "puts the runner's API key in the prompt, and in a sandboxed agent's hands"},
}

// PromptCheck lists what in an agent.md or prompt.md is out of date with
// the runtime layer: one line per problem, quoting the line it is on.
func PromptCheck(content string) []string {
	var out []string
	for _, c := range promptChecks {
		i := strings.Index(content, c.phrase)
		if i < 0 {
			continue
		}
		start := strings.LastIndex(content[:i], "\n") + 1
		end := strings.Index(content[i:], "\n")
		if end < 0 {
			end = len(content) - i
		}
		line := strings.TrimSpace(content[start : i+end])
		if len(line) > 80 {
			line = line[:77] + "..."
		}
		out = append(out, fmt.Sprintf("%q: %s", line, c.problem))
	}
	return out
}
