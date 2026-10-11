package runtime

import (
	"os"
	"path/filepath"
	"slices"
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
	t.Setenv("PATH", filepath.Join(prefix, "bin")+string(os.PathListSeparator)+home)

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
}
