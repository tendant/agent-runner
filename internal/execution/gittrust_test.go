package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
)

func sandboxEnv(t *testing.T) *engineTestEnv {
	t.Helper()
	env := setupTestEnv(t)
	rt, err := sandboxrt.New(sandboxrt.Config{Mode: sandbox.ModePermissive, StateDir: t.TempDir(), Instance: "t",
		NewBackend: func(map[string]string) sandbox.Backend { return nopSandbox{} }})
	if err != nil {
		t.Fatal(err)
	}
	env.handlers.Engine.sandbox = rt
	return env
}

// unpushedRepo is workspace/site cloned from a bare origin, with one local
// commit to push.
func unpushedRepo(t *testing.T, workspace, name string) (bare, repo string) {
	t.Helper()
	base := t.TempDir()
	bare = filepath.Join(base, name+".git")
	repo = filepath.Join(workspace, name)
	git(t, base, "init", "--bare", "-b", "main", bare)
	git(t, base, "clone", bare, repo)
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "T")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "init")
	git(t, repo, "push", "-u", "origin", "HEAD")
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "session work")
	return bare, repo
}

// What the agent writes to .git/config inside the sandbox is not what the
// runner pushes with: the config it saved before the run goes back first.
func TestSandboxedPushUsesSavedGitConfig(t *testing.T) {
	env := sandboxEnv(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(workspace, 0o755)
	bare, repo := unpushedRepo(t, workspace, "site")
	if err := saveGitConfigs(workspace); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))

	// The agent points origin elsewhere.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.git")
	git(t, t.TempDir(), "init", "--bare", "-b", "main", elsewhere)
	git(t, repo, "remote", "set-url", "origin", elsewhere)

	session := runSync(t, env, workspace, func(string) error { return nil })
	if got := git(t, bare, "ls-tree", "--name-only", "main"); !strings.Contains(got, "b.txt") {
		t.Errorf("the session's commit must reach the original origin: %s", got)
	}
	if out := git(t, elsewhere, "for-each-ref"); out != "" {
		t.Errorf("nothing may be pushed to the agent's remote: %s", out)
	}
	if now, _ := os.ReadFile(filepath.Join(repo, ".git", "config")); string(now) != string(saved) {
		t.Errorf("config not restored:\n%s", now)
	}
	if w := session.Snapshot().Warnings; len(w) != 0 {
		t.Errorf("unexpected warnings: %v", w)
	}
}

// A repo the runner did not set up has no saved config: it is not pushed.
func TestSandboxedPushSkipsUnknownRepo(t *testing.T) {
	env := sandboxEnv(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(workspace, 0o755)
	if err := saveGitConfigs(workspace); err != nil {
		t.Fatal(err)
	}
	bare, _ := unpushedRepo(t, workspace, "made-by-agent") // after the save

	session := runSync(t, env, workspace, func(string) error { return nil })
	if got := git(t, bare, "ls-tree", "--name-only", "main"); strings.Contains(got, "b.txt") {
		t.Error("a repo without a saved config must not be pushed")
	}
	var warned bool
	for _, w := range session.Snapshot().Warnings {
		warned = warned || strings.Contains(w, "made-by-agent: not pushed")
	}
	if !warned {
		t.Errorf("expected a warning, got %v", session.Snapshot().Warnings)
	}
}

// .git/config replaced by a link (to another repository's config, say) is
// replaced, not written through.
func TestRestoreGitConfigReplacesLink(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(workspace, 0o755)
	_, repo := unpushedRepo(t, workspace, "site")
	if err := saveGitConfigs(workspace); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other-config")
	os.WriteFile(other, []byte("[core]\n"), 0o644)
	cfg := filepath.Join(repo, ".git", "config")
	os.Remove(cfg)
	os.Symlink(other, cfg)

	trusted, untrusted := restoreGitConfigs(workspace)
	if len(trusted) != 1 || len(untrusted) != 0 {
		t.Fatalf("trusted %v untrusted %v", trusted, untrusted)
	}
	if fi, err := os.Lstat(cfg); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("config should be a plain file again: %v", err)
	}
	if b, _ := os.ReadFile(other); string(b) != "[core]\n" {
		t.Errorf("the link's target was written: %q", b)
	}
}

// Sandboxed cache-back leaves out repos without a saved config and caches
// the others with it.
func TestSandboxedCacheBack(t *testing.T) {
	env := sandboxEnv(t)
	session := filepath.Join(t.TempDir(), "agent-x")
	workspace := filepath.Join(session, "workspace")
	os.MkdirAll(workspace, 0o755)
	_, repo := unpushedRepo(t, workspace, "site")
	if err := saveGitConfigs(workspace); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
	git(t, repo, "config", "remote.origin.url", "/elsewhere")
	unpushedRepo(t, workspace, "made-by-agent")

	env.handlers.cacheReposBack(session)
	cache := env.handlers.config.RepoCacheRoot
	if b, _ := os.ReadFile(filepath.Join(cache, "site", ".git", "config")); string(b) != string(saved) {
		t.Errorf("cached config is not the saved one:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(cache, "made-by-agent")); !os.IsNotExist(err) {
		t.Error("a repo without a saved config must not be cached")
	}
}
