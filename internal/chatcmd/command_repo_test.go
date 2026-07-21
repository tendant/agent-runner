package chatcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeBarRepo creates a bare git repo at path and returns its path.
func makeBarRepo(t *testing.T, path string) string {
	t.Helper()
	if err := exec.Command("git", "init", "--bare", path).Run(); err != nil {
		t.Fatalf("git init --bare %s: %v", path, err)
	}
	return path
}

// makePopulatedRepo creates a non-bare repo with an initial commit, pushes to
// remote, and returns its path.
func makePopulatedRepo(t *testing.T, remote string) string {
	t.Helper()
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", src},
		{"-C", src, "config", "user.email", "test@local"},
		{"-C", src, "config", "user.name", "test"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", src, "add", "-A"},
		{"-C", src, "commit", "-m", "init"},
		{"-C", src, "push", remote, "HEAD:main"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return src
}

func repoCommander(t *testing.T) (*Commander, string) {
	t.Helper()
	env := setupTestEnv(t)
	// Give the Commander a real RepoCacheRoot that exists.
	env.cfg.RepoCacheRoot = env.repoCacheDir
	c := NewCommander(env.cfg, env.rt)
	return c, env.repoCacheDir
}

// --- /repo dispatcher ---

func TestRepo_UnknownSubcommand(t *testing.T) {
	c, _ := repoCommander(t)
	reply := c.handleRepo("bogus")
	if !strings.Contains(reply, "unknown subcommand") {
		t.Errorf("expected unknown subcommand error, got: %s", reply)
	}
}

// --- /repo add ---

func TestRepoAdd_ClonesAndStripsToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)

	reply := c.handleRepoAdd(remote)
	if !strings.Contains(reply, "added myrepo") {
		t.Errorf("unexpected reply: %s", reply)
	}

	// Repo must exist in cache.
	if _, err := os.Stat(filepath.Join(cacheDir, "myrepo", ".git")); err != nil {
		t.Fatalf("repo not cloned into cache: %v", err)
	}

	// Stored remote must be the clean URL (no token).
	out, err := exec.Command("git", "-C", filepath.Join(cacheDir, "myrepo"), "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("get-url: %v", err)
	}
	if strings.Contains(string(out), "oauth2:") {
		t.Errorf("token must not be stored in remote URL, got: %s", out)
	}
}

func TestRepoAdd_AlreadyCached(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)

	c.handleRepoAdd(remote)
	// Second add should report already-in-cache.
	reply := c.handleRepoAdd(remote)
	if !strings.Contains(reply, "already in cache") {
		t.Errorf("expected already-in-cache message, got: %s", reply)
	}
	_ = cacheDir
}

func TestRepoAdd_AppendsToSharedRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, _ := repoCommander(t)
	// Point .env.local writes to a temp dir so they don't pollute the repo.
	defer withTempCWD(t)()

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)

	reply := c.handleRepoAdd(remote)
	if !strings.Contains(reply, "AGENT_SHARED_REPOS") {
		t.Errorf("expected AGENT_SHARED_REPOS note in reply, got: %s", reply)
	}
	found := false
	for _, r := range c.cfg.Agent.SharedRepos {
		if r == "myrepo" {
			found = true
		}
	}
	if !found {
		t.Errorf("myrepo not added to cfg.Agent.SharedRepos: %v", c.cfg.Agent.SharedRepos)
	}
}

func TestRepoAdd_NoDoubleAppendToSharedRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, _ := repoCommander(t)
	defer withTempCWD(t)()
	c.cfg.Agent.SharedRepos = []string{"myrepo"} // already there

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)

	reply := c.handleRepoAdd(remote)
	// Should not mention AGENT_SHARED_REPOS again.
	if strings.Contains(reply, "AGENT_SHARED_REPOS") {
		t.Errorf("should not re-add already-shared repo, got: %s", reply)
	}
}

func TestRepoAdd_EmptyURL(t *testing.T) {
	c, _ := repoCommander(t)
	reply := c.handleRepoAdd("")
	if !strings.Contains(reply, "usage") {
		t.Errorf("expected usage error, got: %s", reply)
	}
}

func TestRepoAdd_CredHelperConfiguredWithToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()
	t.Setenv("GIT_TOKEN", "testtoken123") // read by the credential helper at git-time
	c.cfg.GitToken = "testtoken123"       // live config drives the setup decision

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)
	c.handleRepoAdd(remote)

	out, err := exec.Command("git", "-C", filepath.Join(cacheDir, "myrepo"),
		"config", "--local", "credential.helper").Output()
	if err != nil {
		t.Fatalf("get credential.helper: %v", err)
	}
	if !strings.Contains(string(out), "GIT_TOKEN") {
		t.Errorf("credential.helper should reference GIT_TOKEN, got: %s", out)
	}
}

// --- /repo list ---

func TestRepoList_Empty(t *testing.T) {
	c, _ := repoCommander(t)
	reply := c.handleRepoList()
	if !strings.Contains(reply, "no repos") {
		t.Errorf("expected empty message, got: %s", reply)
	}
}

func TestRepoList_CacheDirNotExist(t *testing.T) {
	c, _ := repoCommander(t)
	c.cfg.RepoCacheRoot = filepath.Join(t.TempDir(), "nonexistent")
	reply := c.handleRepoList()
	if !strings.Contains(reply, "no repos") {
		t.Errorf("expected no-repos message for missing cache dir, got: %s", reply)
	}
}

func TestRepoList_ShowsRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)
	c.handleRepoAdd(remote)

	reply := c.handleRepoList()
	if !strings.Contains(reply, "myrepo") {
		t.Errorf("expected myrepo in list, got: %s", reply)
	}
	_ = cacheDir
}

func TestRepoList_SharedStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()

	// shared-repo: add via handleRepoAdd (auto-appends to SharedRepos).
	remoteA := makeBarRepo(t, t.TempDir()+"/shared-repo.git")
	makePopulatedRepo(t, remoteA)
	c.handleRepoAdd(remoteA)

	// unshared-repo: cloned directly into cache, not in SharedRepos.
	remoteB := makeBarRepo(t, t.TempDir()+"/unshared-repo.git")
	makePopulatedRepo(t, remoteB)
	dst := filepath.Join(cacheDir, "unshared-repo")
	exec.Command("git", "clone", remoteB, dst).Run() //nolint:errcheck

	reply := c.handleRepoList()
	if !strings.Contains(reply, "shared-repo") || !strings.Contains(reply, "shared") {
		t.Errorf("expected shared-repo marked as shared, got:\n%s", reply)
	}
	// Count occurrences of "shared" — shared-repo line should have it, unshared-repo should not.
	lines := strings.Split(reply, "\n")
	for _, line := range lines {
		if strings.Contains(line, "unshared-repo") && strings.Contains(line, "✓ shared") {
			t.Errorf("unshared-repo should not be marked shared, got: %s", line)
		}
	}
}

// --- /repo remove ---

func TestRepoRemove_RemovesDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)
	c.handleRepoAdd(remote)

	reply := c.handleRepoRemove("myrepo")
	if !strings.Contains(reply, "removed myrepo") {
		t.Errorf("unexpected reply: %s", reply)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "myrepo")); !os.IsNotExist(err) {
		t.Error("expected repo dir to be gone")
	}
}

func TestRepoRemove_NotFound_Suggests(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()

	// Create a cached repo named "my-repo" (with hyphen).
	remote := makeBarRepo(t, t.TempDir()+"/my-repo.git")
	makePopulatedRepo(t, remote)
	c.handleRepoAdd(remote)

	// Ask to remove "my_repo" (with underscore) — should suggest "my-repo".
	reply := c.handleRepoRemove("my_repo")
	if !strings.Contains(reply, "did you mean") {
		t.Errorf("expected typo suggestion, got: %s", reply)
	}
	_ = cacheDir
}

func TestRepoRemove_EmptyName(t *testing.T) {
	c, _ := repoCommander(t)
	reply := c.handleRepoRemove("")
	if !strings.Contains(reply, "usage") {
		t.Errorf("expected usage error, got: %s", reply)
	}
}

func TestRepoRemove_DropsFromSharedRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, _ := repoCommander(t)
	defer withTempCWD(t)()

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)

	// Add — which appends to AGENT_SHARED_REPOS.
	c.handleRepoAdd(remote)
	if len(c.cfg.Agent.SharedRepos) == 0 {
		t.Fatal("expected myrepo in SharedRepos after add")
	}

	// Remove — should drop it from AGENT_SHARED_REPOS.
	reply := c.handleRepoRemove("myrepo")
	if !strings.Contains(reply, "AGENT_SHARED_REPOS") {
		t.Errorf("expected AGENT_SHARED_REPOS note in reply, got: %s", reply)
	}
	for _, r := range c.cfg.Agent.SharedRepos {
		if r == "myrepo" {
			t.Errorf("myrepo should have been removed from SharedRepos, still present: %v", c.cfg.Agent.SharedRepos)
		}
	}
}

func TestRepoRemove_NoSharedNote_WhenNotShared(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, _ := repoCommander(t)
	defer withTempCWD(t)()
	// Repo is in cache but NOT in SharedRepos.
	c.cfg.Agent.SharedRepos = []string{"other-repo"}

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)
	// Clone directly into cache without going through handleRepoAdd.
	exec.Command("git", "clone", remote, filepath.Join(c.cfg.RepoCacheRoot, "myrepo")).Run() //nolint:errcheck

	reply := c.handleRepoRemove("myrepo")
	if strings.Contains(reply, "AGENT_SHARED_REPOS") {
		t.Errorf("should not mention AGENT_SHARED_REPOS when repo was not shared, got: %s", reply)
	}
	if !strings.Contains(reply, "removed myrepo") {
		t.Errorf("expected removed message, got: %s", reply)
	}
}

// --- /repo update ---

func TestRepoUpdate_ResetsToOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	src := makePopulatedRepo(t, remote)

	c.handleRepoAdd(remote)

	// Push a new commit to remote from src.
	if err := os.WriteFile(filepath.Join(src, "new.md"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", src, "add", "-A"},
		{"-C", src, "commit", "-m", "second"},
		{"-C", src, "push", remote, "HEAD:main"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	reply := c.handleRepoUpdate("myrepo")
	if !strings.Contains(reply, "updated myrepo") {
		t.Errorf("unexpected reply: %s", reply)
	}

	// new.md should be present in cache after update.
	if _, err := os.Stat(filepath.Join(cacheDir, "myrepo", "new.md")); err != nil {
		t.Errorf("expected new.md after update: %v", err)
	}
}

func TestRepoUpdate_NotFound_Suggests(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()

	remote := makeBarRepo(t, t.TempDir()+"/my-repo.git")
	makePopulatedRepo(t, remote)
	c.handleRepoAdd(remote)

	reply := c.handleRepoUpdate("my_repo")
	if !strings.Contains(reply, "did you mean") {
		t.Errorf("expected typo suggestion, got: %s", reply)
	}
	_ = cacheDir
}

func TestRepoUpdate_EmptyName(t *testing.T) {
	c, _ := repoCommander(t)
	reply := c.handleRepoUpdate("")
	if !strings.Contains(reply, "usage") {
		t.Errorf("expected usage error, got: %s", reply)
	}
}

func TestRepoUpdate_SetsCredHelperWhenMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c, cacheDir := repoCommander(t)
	defer withTempCWD(t)()
	t.Setenv("GIT_TOKEN", "testtoken") // read by the credential helper at git-time
	c.cfg.GitToken = "testtoken"       // live config drives the setup decision

	remote := makeBarRepo(t, t.TempDir()+"/myrepo.git")
	makePopulatedRepo(t, remote)
	c.handleRepoAdd(remote)

	// Clear the credential helper to simulate a manually-cloned repo.
	exec.Command("git", "-C", filepath.Join(cacheDir, "myrepo"), "config", "--unset", "credential.helper").Run() //nolint:errcheck

	// Update — should re-configure the helper.
	c.handleRepoUpdate("myrepo")

	out, err := exec.Command("git", "-C", filepath.Join(cacheDir, "myrepo"), "config", "--local", "credential.helper").Output()
	if err != nil || !strings.Contains(string(out), "GIT_TOKEN") {
		t.Errorf("expected local credential.helper to reference GIT_TOKEN after update, got: %q (err: %v)", out, err)
	}
}
