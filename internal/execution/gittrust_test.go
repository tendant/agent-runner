package execution

import (
	"os"
	"os/exec"
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

// Sandboxed, the agent's session is closed before the runner's git, and
// started again for a conflict: the conflict still gets resolved.
func TestSandboxedConflictRestartsAgent(t *testing.T) {
	env := sandboxEnv(t)
	bare, workspace := conflictedWorkspace(t)
	repo := filepath.Join(workspace, "site")
	if err := saveGitConfigs(workspace); err != nil {
		t.Fatal(err)
	}
	session := runSync(t, env, workspace, func(string) error {
		os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("mine+theirs\n"), 0o644)
		git(t, repo, "add", "shared.txt")
		cmd := exec.Command("git", "rebase", "--continue")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rebase --continue: %v\n%s", err, out)
		}
		return nil
	})
	if got := git(t, bare, "show", "main:shared.txt"); got != "mine+theirs" {
		t.Errorf("origin should have the merged result, got %q (warnings %v)", got, session.Snapshot().Warnings)
	}
}

// hasCommits reads files only.
func TestHasCommitsWithoutGit(t *testing.T) {
	empty := t.TempDir()
	exec.Command("git", "-C", empty, "init", "-q").Run()
	if hasCommits(empty) {
		t.Error("an empty repo has no commits")
	}
	full := t.TempDir()
	exec.Command("git", "-C", full, "init", "-q").Run()
	exec.Command("git", "-C", full, "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "x").Run()
	if !hasCommits(full) {
		t.Error("a repo with a commit")
	}
}

// A repo whose .git points git elsewhere is not pushed.
func TestSandboxedPushRefusesRedirectedGitDir(t *testing.T) {
	env := sandboxEnv(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(workspace, 0o755)
	bare, repo := unpushedRepo(t, workspace, "site")
	if err := saveGitConfigs(workspace); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, ".git", "commondir"), []byte(t.TempDir()+"\n"), 0o644)
	session := runSync(t, env, workspace, func(string) error { return nil })
	if got := git(t, bare, "ls-tree", "--name-only", "main"); strings.Contains(got, "b.txt") {
		t.Error("a repo with a planted commondir must not be pushed")
	}
	var warned bool
	for _, w := range session.Snapshot().Warnings {
		warned = warned || strings.Contains(w, "site: not pushed")
	}
	if !warned {
		t.Errorf("expected a warning: %v", session.Snapshot().Warnings)
	}
}

// A rebase the agent left in progress (resolved, not continued) is never
// continued on the host: it is aborted and redone, and work that still
// conflicts is parked, with main untouched.
func TestRebaseLeftInProgressIsNotContinued(t *testing.T) {
	env := sandboxEnv(t)
	bare, workspace := conflictedWorkspace(t)
	repo := filepath.Join(workspace, "site")
	if err := saveGitConfigs(workspace); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "pull", "--rebase", "origin", "main")
	cmd.Dir = repo
	cmd.Run() // stops on the conflict
	os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("resolved but not continued\n"), 0o644)
	git(t, repo, "add", "shared.txt")

	session := runSync(t, env, workspace, func(string) error { return nil })
	if got := git(t, bare, "show", "main:shared.txt"); got != "theirs" {
		t.Errorf("main must not get the uncontinued resolution: %q", got)
	}
	if got := git(t, bare, "show", "agent-rescue/"+session.ID+":shared.txt"); got != "mine" {
		t.Errorf("the session's own commit should be parked: %q", got)
	}
}
