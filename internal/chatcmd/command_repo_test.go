package chatcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/repos"
)

func init() { repos.AllowLocalPaths = true }

// originRepo is a bare repo with one commit on main and a develop branch.
func originRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "site.git")
	src := filepath.Join(base, "src")
	for _, args := range [][]string{
		{"init", "-q", "--bare", "-b", "main", origin},
		{"init", "-q", "-b", "main", src},
		{"-C", src, "config", "user.email", "t@t"},
		{"-C", src, "config", "user.name", "T"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hi"), 0o644)
	for _, args := range [][]string{
		{"-C", src, "add", "-A"}, {"-C", src, "commit", "-qm", "init"},
		{"-C", src, "push", "-q", origin, "HEAD:main"}, {"-C", src, "push", "-q", origin, "HEAD:develop"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return origin
}

func newRepoCommander(t *testing.T) *Commander {
	t.Helper()
	env := setupTestEnv(t)
	dir := t.TempDir()
	env.cfg.ReposFile = filepath.Join(dir, "repos.json")
	env.cfg.SecretsFile = filepath.Join(dir, "secrets.json")
	env.cfg.RepoCacheRoot = filepath.Join(dir, "cache")
	return NewCommander(env.cfg, env.rt)
}

func handle(t *testing.T, c *Commander, cmd string) string {
	t.Helper()
	reply, _, ok := c.Handle(cmd, nil)
	if !ok {
		t.Fatalf("%s not handled", cmd)
	}
	return reply
}

func TestRepoAddListSetRemove(t *testing.T) {
	c := newRepoCommander(t)
	origin := originRepo(t)
	if r := handle(t, c, "/repo"); !strings.Contains(r, "no repos") {
		t.Errorf("empty: %q", r)
	}
	r := handle(t, c, "/repo add "+origin+" branch=develop push=false")
	if !strings.HasPrefix(r, "ok: site added") || !strings.Contains(r, "develop") || !strings.Contains(r, "read-only") {
		t.Fatalf("add: %q", r)
	}
	if _, err := os.Stat(repos.CachePath(c.cfg.RepoCacheRoot, "site")); err != nil {
		t.Errorf("add should fill the cache: %v", err)
	}
	if r := handle(t, c, "/repo add "+origin); !strings.Contains(r, "already registered") {
		t.Errorf("duplicate: %q", r)
	}
	if r := handle(t, c, "/repo list"); !strings.Contains(r, "- site: "+origin) {
		t.Errorf("list: %q", r)
	}
	if r := handle(t, c, "/repo set site push=true branch=main"); !strings.Contains(r, "pushed") {
		t.Errorf("set: %q", r)
	}
	if r := handle(t, c, "/repo set site branch=nope"); !strings.Contains(r, "no branch nope") {
		t.Errorf("bad branch: %q", r)
	}
	if r := handle(t, c, "/repo remove site"); !strings.HasPrefix(r, "ok") {
		t.Errorf("remove: %q", r)
	}
	if _, err := os.Stat(repos.CachePath(c.cfg.RepoCacheRoot, "site")); !os.IsNotExist(err) {
		t.Error("remove should drop the cache")
	}
}

func TestRepoAddRefusals(t *testing.T) {
	c := newRepoCommander(t)
	for cmd, want := range map[string]string{
		"/repo add https://lei:tok@gitee.com/o/x.git":            "password",
		"/repo add https://gitee.com/o/x.git secret=GIT_TOKEN":   "agent-runner's own",
		"/repo add https://gitee.com/o/x.git colour=red":         "unknown option",
		"/repo add " + filepath.Join(t.TempDir(), "missing.git"): "clone",
		"/repo add https://gitee.com/o/x.git secret=GITEE_TOKEN": "/secret set GITEE_TOKEN",
		"/repo bogus": "unknown /repo command",
	} {
		if r := handle(t, c, cmd); !strings.Contains(r, want) {
			t.Errorf("%s: want %q in %q", cmd, want, r)
		}
	}
}

// makeBarRepo creates a bare git repo at path and returns its path.
func makeBarRepo(t *testing.T, path string) string {
	t.Helper()
	if err := exec.Command("git", "init", "--bare", path).Run(); err != nil {
		t.Fatalf("git init --bare %s: %v", path, err)
	}
	return path
}

func TestHelpTopics(t *testing.T) {
	c := newRepoCommander(t)
	for cmd, want := range map[string]string{
		"/help":        "Getting started",
		"/help secret": "BEGIN OPENSSH PRIVATE KEY",
		"/help repo":   "push=false — read-only",
		"/help memory": "/memory undo",
		"/repo help":   "secret=NAME",
		"/help nope":   "/help secret",
	} {
		if r := handle(t, c, cmd); !strings.Contains(r, want) {
			t.Errorf("%s lacks %q", cmd, want)
		}
	}
}
