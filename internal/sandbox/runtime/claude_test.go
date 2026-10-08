package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSeedClaudeConfigCopiesTheAllowlistOnly(t *testing.T) {
	host := t.TempDir()
	src := filepath.Join(host, ".claude")
	write(t, filepath.Join(src, "settings.json"), `{"model":"opus"}`)
	write(t, filepath.Join(src, "CLAUDE.md"), "be brief")
	write(t, filepath.Join(src, "skills", "deploy", "SKILL.md"), "skill")
	write(t, filepath.Join(src, ".credentials.json"), `{"secret":1}`)
	write(t, filepath.Join(src, "projects", "other", "transcript.jsonl"), "private")
	write(t, filepath.Join(src, "history.jsonl"), "private")
	state := filepath.Join(host, ".claude.json")
	write(t, state, `{"mcpServers":{"fs":{"command":"mcp-fs"}},"hasCompletedOnboarding":true,"oauthAccount":{"email":"x"},"projects":{"/p":{}}}`)

	dst := filepath.Join(t.TempDir(), ".claude")
	// The run's own state from an earlier turn is kept.
	write(t, filepath.Join(dst, "projects", "ws", "session.jsonl"), "resume me")
	write(t, filepath.Join(dst, "commands", "stale.md"), "gone from the host")

	if err := seedClaudeConfig(ClaudeSeed{Dir: src, StateFile: state}, dst); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"settings.json", "CLAUDE.md", "skills/deploy/SKILL.md", "projects/ws/session.jsonl"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	for _, never := range []string{".credentials.json", "projects/other", "history.jsonl", "commands/stale.md"} {
		if _, err := os.Stat(filepath.Join(dst, never)); err == nil {
			t.Errorf("%s must not be in the sandbox's config dir", never)
		}
	}
	b, err := os.ReadFile(filepath.Join(dst, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	if got["mcpServers"] == nil || got["hasCompletedOnboarding"] != true {
		t.Errorf(".claude.json = %s", b)
	}
	if got["oauthAccount"] != nil || got["projects"] != nil {
		t.Errorf(".claude.json leaked account or project history: %s", b)
	}
}

func TestSeedClaudeConfigWithoutASource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".claude")
	if err := seedClaudeConfig(ClaudeSeed{}, dst); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		t.Errorf("config dir not created: %v", err)
	}
	missing := ClaudeSeed{Dir: filepath.Join(dst, "nope"), StateFile: filepath.Join(dst, "nope.json")}
	if err := seedClaudeConfig(missing, dst); err != nil {
		t.Errorf("a missing source is not an error: %v", err)
	}
}

func TestSeedClaudeConfigFollowsSymlinksWithoutLooping(t *testing.T) {
	host := t.TempDir()
	src := filepath.Join(host, ".claude")
	shared := filepath.Join(host, "dotfiles", "skills")
	write(t, filepath.Join(shared, "deploy", "SKILL.md"), "skill")
	os.MkdirAll(src, 0o700)
	if err := os.Symlink(shared, filepath.Join(src, "skills")); err != nil { // the whole entry is a link
		t.Fatal(err)
	}
	os.Symlink(shared, filepath.Join(shared, "loop"))                          // and loops back
	os.Symlink(filepath.Join(host, "gone"), filepath.Join(shared, "dangling")) // and dangles

	dst := filepath.Join(t.TempDir(), ".claude")
	if err := seedClaudeConfig(ClaudeSeed{Dir: src}, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "skills"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Fatalf("skills must be a real copy: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "skills", "deploy", "SKILL.md")); err != nil {
		t.Error(err)
	}
}
