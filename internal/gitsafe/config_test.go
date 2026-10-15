package gitsafe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRepo(t *testing.T) {
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q").Run()
	if err := CheckRepo(repo); err != nil {
		t.Fatalf("a plain repo: %v", err)
	}
	for _, name := range redirecting {
		p := filepath.Join(repo, ".git", name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("elsewhere\n"), 0o644)
		if err := CheckRepo(repo); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
		os.Remove(p)
	}
	cfg := filepath.Join(repo, ".git", "config")
	os.Rename(cfg, cfg+".real")
	os.Symlink(cfg+".real", cfg)
	if err := CheckRepo(repo); err == nil {
		t.Error("a linked config must be refused")
	}
	linked := t.TempDir()
	os.Symlink(filepath.Join(repo, ".git"), filepath.Join(linked, ".git"))
	if err := CheckRepo(linked); err == nil {
		t.Error("a linked .git must be refused")
	}
}
