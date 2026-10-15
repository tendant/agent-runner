package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/repos"
)

// A session gets every registered repo checked out, and agent-runner pushes
// the agent's commits to the pushed ones only.
func TestSessionChecksOutAndPushesRegisteredRepos(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	h.config.ReposFile = filepath.Join(t.TempDir(), "repos.json")
	h.config.RepoCacheRoot = filepath.Join(t.TempDir(), "cache")
	repos.AllowLocalPaths = true

	base := t.TempDir()
	origin := func(name string) string {
		bare := filepath.Join(base, name+".git")
		src := filepath.Join(base, name+"-src")
		git(t, base, "init", "-q", "--bare", "-b", "main", bare)
		git(t, base, "clone", "-q", bare, src)
		git(t, src, "config", "user.email", "t@t")
		git(t, src, "config", "user.name", "T")
		os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o644)
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "init")
		git(t, src, "push", "-q", "origin", "HEAD")
		return bare
	}
	site, docs := origin("site"), origin("docs")
	reg := repos.New(h.config.ReposFile)
	reg.Put(repos.Repo{Name: "site", URL: site, Push: true})
	reg.Put(repos.Repo{Name: "docs", URL: docs, Push: false})

	var prompt string
	h.executor = funcExecutor{fn: func(ws string) error {
		for _, name := range []string{"site", "docs"} {
			dir := filepath.Join(ws, name)
			git(t, dir, "config", "user.email", "a@a")
			git(t, dir, "config", "user.name", "A")
			os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-qm", "agent work")
		}
		// A repo the agent made itself.
		own := filepath.Join(ws, "scratch")
		os.MkdirAll(own, 0o755)
		exec.Command("git", "-C", own, "init", "-q").Run()
		return nil
	}}
	_ = prompt
	session, _ := h.agentManager.CreateSession("work", nil, "test", "", 1, 60)
	h.ExecuteAgent(session)

	if got := git(t, site, "ls-tree", "--name-only", "main"); !strings.Contains(got, "b.txt") {
		t.Errorf("site: the agent's commit must be pushed: %s", got)
	}
	if got := git(t, docs, "ls-tree", "--name-only", "main"); strings.Contains(got, "b.txt") {
		t.Error("docs is read-only: never pushed")
	}
	warnings := strings.Join(session.Snapshot().Warnings, "\n")
	if !strings.Contains(warnings, "docs: commits not pushed: the repo is read-only") {
		t.Errorf("read-only warning missing:\n%s", warnings)
	}
	if _, err := os.Stat(repos.CachePath(h.config.RepoCacheRoot, "site")); err != nil {
		t.Errorf("the cache should hold the bare clone: %v", err)
	}
	// The cache is never written back from the workspace.
	if out := git(t, repos.CachePath(h.config.RepoCacheRoot, "site"), "log", "--format=%s", "main"); strings.Contains(out, "agent work") && !strings.Contains(git(t, site, "log", "--format=%s", "main"), "agent work") {
		t.Error("cache holds the agent's commit without origin having it")
	}
}
