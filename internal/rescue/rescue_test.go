package rescue

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A session workspace: <dir>/workspace holds the agent's repos.
func session(t *testing.T) (dir, ws string) {
	dir = t.TempDir()
	ws = filepath.Join(dir, "workspace")
	os.MkdirAll(ws, 0o755)
	return dir, ws
}

// Unpushed commits and uncommitted work in a registered repo are kept and
// can be restored; regenerated folders are not.
func TestSave_UnpushedAndUncommitted(t *testing.T) {
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	git(t, base, "init", "-q", "--bare", "-b", "main", remote)
	dir, ws := session(t)
	app := filepath.Join(ws, "app")
	git(t, ws, "clone", "-q", remote, "app")
	write(t, filepath.Join(app, "keep.go"), "package main\n")
	write(t, filepath.Join(app, "gone.go"), "package main\n")
	git(t, app, "add", "-A")
	git(t, app, "commit", "-q", "-m", "first")
	git(t, app, "push", "-q", "origin", "HEAD:main")
	for i, msg := range []string{"m1", "m2"} {
		write(t, filepath.Join(app, "feature.go"), "package main // "+string(rune('a'+i))+"\n")
		git(t, app, "add", "-A")
		git(t, app, "commit", "-q", "-m", msg)
	}
	write(t, filepath.Join(app, "keep.go"), "package main // edited, not committed\n")
	write(t, filepath.Join(app, "new.go"), "package main // never committed\n")
	os.Remove(filepath.Join(app, "gone.go"))
	write(t, filepath.Join(app, "node_modules", "x", "index.js"), "regenerated")

	rep, err := Save(dir, filepath.Join(base, "rescued"), "session-1")
	if err != nil || rep.Empty() || len(rep.Repos) != 1 {
		t.Fatalf("report %+v, err %v", rep, err)
	}
	rp := rep.Repos[0]
	if rp.Commits != 2 || rp.Bundle == "" || rp.Complete {
		t.Fatalf("commits: %+v", rp)
	}
	slices.Sort(rp.Changed)
	if !slices.Equal(rp.Changed, []string{"keep.go", "new.go"}) || !slices.Equal(rp.Deleted, []string{"gone.go"}) {
		t.Fatalf("changed %v deleted %v", rp.Changed, rp.Deleted)
	}
	if _, err := os.Stat(filepath.Join(rep.Dir, "repos", "app", "node_modules")); err == nil {
		t.Error("node_modules was kept")
	}
	if b, _ := os.ReadFile(filepath.Join(rep.Dir, "repos", "app", "keep.go")); !strings.Contains(string(b), "edited, not committed") {
		t.Errorf("keep.go kept as %q", b)
	}
	// The bundle restores the commits into a clone of the remote.
	restore := filepath.Join(base, "restore")
	git(t, base, "clone", "-q", remote, restore)
	git(t, restore, "fetch", "-q", rp.Bundle, "refs/heads/*:refs/heads/rescued/*")
	if log := git(t, restore, "log", "--format=%s", "rescued/main"); !strings.Contains(log, "m2") || !strings.Contains(log, "m1") {
		t.Fatalf("restored log:\n%s", log)
	}
	if readme, _ := os.ReadFile(filepath.Join(rep.Dir, "README.txt")); !strings.Contains(string(readme), "git fetch") {
		t.Errorf("README:\n%s", readme)
	}
}

// A clean, pushed repo leaves nothing behind.
func TestSave_NothingToKeep(t *testing.T) {
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	git(t, base, "init", "-q", "--bare", "-b", "main", remote)
	dir, ws := session(t)
	git(t, ws, "clone", "-q", remote, "app")
	app := filepath.Join(ws, "app")
	write(t, filepath.Join(app, "a.go"), "package a\n")
	git(t, app, "add", "-A")
	git(t, app, "commit", "-q", "-m", "a")
	git(t, app, "push", "-q", "origin", "HEAD:main")
	write(t, filepath.Join(ws, "_progress.json"), `{"status":"done"}`) // the runner's
	rescued := filepath.Join(base, "rescued")
	rep, err := Save(dir, rescued, "session-2")
	if err != nil || !rep.Empty() {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if entries, _ := os.ReadDir(rescued); len(entries) != 0 {
		t.Fatalf("left %d entries for a clean workspace", len(entries))
	}
}

// A repo the agent made itself (no remote, so nobody pushes it) and files
// outside any repo are kept whole.
func TestSave_UnregisteredRepoAndLooseFiles(t *testing.T) {
	base := t.TempDir()
	dir, ws := session(t)
	own := filepath.Join(ws, "slow-town")
	git(t, ws, "init", "-q", "-b", "main", "slow-town")
	write(t, filepath.Join(own, "main.go"), "package main\n")
	git(t, own, "add", "-A")
	git(t, own, "commit", "-q", "-m", "M0")
	write(t, filepath.Join(ws, "notes", "plan.md"), "# M1\n")
	write(t, filepath.Join(ws, "_send", "out.txt"), "delivered") // the runner's

	rep, err := Save(dir, filepath.Join(base, "rescued"), "task-x")
	if err != nil || len(rep.Repos) != 1 || !rep.Repos[0].Complete || rep.Repos[0].Commits != 1 {
		t.Fatalf("report %+v err %v", rep, err)
	}
	clone := filepath.Join(base, "clone")
	git(t, base, "clone", "-q", rep.Repos[0].Bundle, clone)
	if _, err := os.Stat(filepath.Join(clone, "main.go")); err != nil {
		t.Fatal("the bundle does not restore the repo")
	}
	if !slices.Equal(rep.Loose, []string{filepath.Join("notes", "plan.md")}) {
		t.Fatalf("loose %v", rep.Loose)
	}
	if !strings.Contains(rep.Summary(), "1 unpushed commit") || !strings.Contains(rep.Summary(), "outside any repo") {
		t.Errorf("summary %q", rep.Summary())
	}
}

// The agent's repo config can't make the runner run its programs: no git
// status/diff (clean filters, textconv), hooks and fsmonitor disabled.
func TestSave_RunsNoAgentProgram(t *testing.T) {
	base := t.TempDir()
	dir, ws := session(t)
	repo := filepath.Join(ws, "evil")
	git(t, ws, "init", "-q", "-b", "main", "evil")
	marker := filepath.Join(base, "pwned")
	write(t, filepath.Join(repo, ".gitattributes"), "* filter=x diff=x\n")
	write(t, filepath.Join(repo, "a.txt"), "a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a")
	for _, kv := range [][2]string{
		{"filter.x.clean", "touch " + marker + "; cat"},
		{"diff.x.textconv", "touch " + marker + "; cat"},
		{"core.fsmonitor", "touch " + marker},
	} {
		git(t, repo, "config", kv[0], kv[1])
	}
	write(t, filepath.Join(repo, "a.txt"), "changed\n")
	if _, err := Save(dir, filepath.Join(base, "rescued"), "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the rescue ran a program the agent's repo config named")
	}
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	old, recent := filepath.Join(root, "old"), filepath.Join(root, "recent")
	os.MkdirAll(old, 0o700)
	os.MkdirAll(recent, 0o700)
	past := time.Now().Add(-40 * 24 * time.Hour)
	os.Chtimes(old, past, past)
	Prune(root, 30*24*time.Hour)
	if _, err := os.Stat(old); err == nil {
		t.Error("an old rescue was kept")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("a recent rescue was removed")
	}
}
