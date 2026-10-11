package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestToolReadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// npm-style: prefix/bin/codex -> ../lib/node_modules/@openai/codex/bin/codex.js
	prefix := t.TempDir()
	pkg := filepath.Join(prefix, "lib", "node_modules", "@openai", "codex", "bin")
	os.MkdirAll(pkg, 0o755)
	os.WriteFile(filepath.Join(pkg, "codex.js"), []byte("#!/bin/sh\n"), 0o755)
	os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
	os.Symlink(filepath.Join(pkg, "codex.js"), filepath.Join(prefix, "bin", "codex"))
	// A tool right in the home directory must not make the home readable.
	os.WriteFile(filepath.Join(home, "claude"), []byte("#!/bin/sh\n"), 0o755)
	// ~/.local/bin/opencode: ~/.local/bin, never all of ~/.local (whose
	// share/ holds credentials); a deeper prefix (~/.local/node) is fine.
	local := filepath.Join(home, ".local")
	os.MkdirAll(filepath.Join(local, "bin"), 0o755)
	os.MkdirAll(filepath.Join(local, "share", "opencode"), 0o700)
	os.WriteFile(filepath.Join(local, "bin", "opencode"), []byte("#!/bin/sh\n"), 0o755)
	os.MkdirAll(filepath.Join(local, "node", "bin"), 0o755)
	os.WriteFile(filepath.Join(local, "node", "bin", "node"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", strings.Join([]string{filepath.Join(prefix, "bin"), home, filepath.Join(local, "bin"), filepath.Join(local, "node", "bin")}, string(os.PathListSeparator)))

	got := toolReadable()
	real, _ := filepath.EvalSymlinks(prefix) // macOS: /var -> /private/var
	realHome, _ := filepath.EvalSymlinks(home)
	for _, want := range []string{filepath.Join(prefix, "bin"), filepath.Join(real, "lib", "node_modules"),
		filepath.Join(real, "lib", "node_modules", "@openai", "codex", "bin")} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if slices.Contains(got, home) || slices.Contains(got, realHome) {
		t.Errorf("home directory made readable: %v", got)
	}
	realLocal := filepath.Join(realHome, ".local")
	if slices.Contains(got, local) || slices.Contains(got, realLocal) {
		t.Errorf("all of ~/.local made readable: %v", got)
	}
	for _, want := range []string{filepath.Join(local, "bin"), filepath.Join(realLocal, "node")} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}
