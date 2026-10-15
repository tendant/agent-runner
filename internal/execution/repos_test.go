package execution

import (
	"errors"
	"github.com/agent-runner/agent-runner/internal/agent"
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

// A session that aborts (a permanent provider error) still pushes what the
// agent committed before, and still delivers _send/.
func TestAbortedSessionKeepsWork(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	h.config.RepoCacheRoot = filepath.Join(t.TempDir(), "cache")
	repos.AllowLocalPaths = true
	base := t.TempDir()
	bare := filepath.Join(base, "site.git")
	src := filepath.Join(base, "src")
	git(t, base, "init", "-q", "--bare", "-b", "main", bare)
	git(t, base, "clone", "-q", bare, src)
	git(t, src, "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init")
	git(t, src, "push", "-q", "origin", "HEAD")
	repos.New(h.config.ReposFile).Put(repos.Repo{Name: "site", URL: bare, Push: true})

	calls := 0
	h.executor = funcExecutor{fn: func(ws string) error {
		calls++
		if calls == 1 {
			dir := filepath.Join(ws, "site")
			os.WriteFile(filepath.Join(dir, "work.txt"), []byte("w\n"), 0o644)
			git(t, dir, "add", "-A")
			git(t, dir, "-c", "user.email=a@a", "-c", "user.name=A", "commit", "-qm", "agent work")
			os.MkdirAll(filepath.Join(ws, "_send"), 0o755)
			os.WriteFile(filepath.Join(ws, "_send", "report.txt"), []byte("r\n"), 0o644)
		}
		return errors.New("Your credit balance is too low to access the API")
	}}
	session, _ := h.agentManager.CreateSession("work", nil, "test", "", 5, 60)
	h.ExecuteAgent(session)

	snap := session.Snapshot()
	if snap.Status != agent.SessionStatusFailed {
		t.Fatalf("status %s, want failed", snap.Status)
	}
	if got := git(t, bare, "ls-tree", "--name-only", "main"); !strings.Contains(got, "work.txt") {
		t.Errorf("the agent's commit must be pushed even though the session aborted: %s", got)
	}
	var delivered bool
	for _, f := range snap.OutputFiles {
		delivered = delivered || strings.Contains(f.Name, "report.txt")
	}
	if !delivered {
		t.Errorf("_send/ must still be delivered: %+v", snap.OutputFiles)
	}
}
