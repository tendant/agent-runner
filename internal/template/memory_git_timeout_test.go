package template

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A memory git command that hangs is ended: it runs under the memory lock.
func TestMemoryGitCommandTimesOut(t *testing.T) {
	old := memoryGitTimeout
	memoryGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { memoryGitTimeout = old })
	start := time.Now()
	err := gitRunEnv(t.TempDir(), nil, "-c", "alias.hang=!sleep 20", "hang")
	if err == nil {
		t.Fatal("a hanging git command succeeded")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the hanging command ran for %v", d)
	}
}

// Two hosts append to the same memory file: syncing keeps both hosts'
// lines (union merge), never just one side's.
func TestMemorySync_KeepsBothSides(t *testing.T) {
	base := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	remote := filepath.Join(base, "remote.git")
	run(base, "init", "-q", "--bare", "-b", "main", remote)
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	run(base, "clone", "-q", remote, a)
	os.WriteFile(filepath.Join(a, "lessons.md"), []byte("# Lessons\n"), 0o644)
	run(a, "add", "-A")
	run(a, "commit", "-q", "-m", "start")
	run(a, "push", "-q", "-u", "origin", "HEAD:main")
	run(base, "clone", "-q", remote, b)
	for _, dir := range []string{a, b} {
		run(dir, "config", "user.name", "t")
		run(dir, "config", "user.email", "t@t")
	}

	appendLine := func(dir, line string) {
		f, _ := os.OpenFile(filepath.Join(dir, "lessons.md"), os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(line + "\n")
		f.Close()
	}
	appendLine(a, "- host A: deploys need the QA tag")
	run(a, "commit", "-q", "-am", "A's lesson")
	run(a, "push", "-q", "origin", "HEAD:main")

	appendLine(b, "- host B: the alerts page defaults to 30 days")
	if err := CommitAndPushMemory(b, MemoryGitCreds{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := run(base, "--git-dir", remote, "show", "main:lessons.md")
	for _, want := range []string{"host A: deploys need the QA tag", "host B: the alerts page defaults to 30 days"} {
		if !strings.Contains(got, want) {
			t.Errorf("remote lessons.md lost %q:\n%s", want, got)
		}
	}
}
