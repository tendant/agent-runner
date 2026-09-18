package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// twoClones sets up a bare origin with one commit and two clones of it.
func twoClones(t *testing.T) (bare, a, b string) {
	t.Helper()
	base := t.TempDir()
	bare = filepath.Join(base, "origin.git")
	a = filepath.Join(base, "a")
	b = filepath.Join(base, "b")
	run(t, base, "init", "--bare", "-b", "main", bare)
	run(t, base, "clone", bare, a)
	for _, d := range []string{a} {
		run(t, d, "config", "user.email", "t@t")
		run(t, d, "config", "user.name", "T")
	}
	os.WriteFile(filepath.Join(a, "shared.txt"), []byte("line1\nline2\n"), 0o644)
	run(t, a, "add", "-A")
	run(t, a, "commit", "-m", "init")
	run(t, a, "push", "-u", "origin", "HEAD")
	run(t, base, "clone", bare, b)
	run(t, b, "config", "user.email", "t@t")
	run(t, b, "config", "user.name", "T")
	return
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-m", msg)
}

func TestPush_RebasesOntoMovedRemote(t *testing.T) {
	bare, a, b := twoClones(t)
	commitFile(t, b, "other.txt", "b\n", "b: other")
	run(t, b, "push", "origin", "HEAD")
	commitFile(t, a, "mine.txt", "a\n", "a: mine")

	ops := NewOperations(3, 0)
	if err := ops.Push(context.Background(), a); err != nil {
		t.Fatalf("push should rebase and succeed: %v", err)
	}
	log := run(t, bare, "log", "--oneline", "main")
	if !strings.Contains(log, "a: mine") || !strings.Contains(log, "b: other") {
		t.Errorf("origin should have both commits:\n%s", log)
	}
	if ops.RebaseInProgress(context.Background(), a) {
		t.Error("no rebase should be left in progress")
	}
}

func TestPush_ConflictLeavesRebaseForResolution(t *testing.T) {
	bare, a, b := twoClones(t)
	commitFile(t, b, "shared.txt", "line1 from b\nline2\n", "b: edit shared")
	run(t, b, "push", "origin", "HEAD")
	commitFile(t, a, "shared.txt", "line1 from a\nline2\n", "a: edit shared")

	ops := NewOperations(3, 0)
	err := ops.Push(context.Background(), a)
	var rc *RebaseConflictError
	if !errors.As(err, &rc) {
		t.Fatalf("expected RebaseConflictError, got %v", err)
	}
	if len(rc.Files) != 1 || rc.Files[0] != "shared.txt" || rc.Branch != "main" || rc.RepoPath != a {
		t.Errorf("conflict details wrong: %+v", rc)
	}
	if !ops.RebaseInProgress(context.Background(), a) {
		t.Fatal("rebase should be left in progress for the agent")
	}
	if name, _ := ops.RebaseHeadName(context.Background(), a); name != "main" {
		t.Errorf("RebaseHeadName = %q", name)
	}

	// Resolve the way the agent is told to, then push again.
	os.WriteFile(filepath.Join(a, "shared.txt"), []byte("line1 from a and b\nline2\n"), 0o644)
	run(t, a, "add", "shared.txt")
	if err := ops.ContinueRebase(context.Background(), a); err != nil {
		t.Fatalf("continue: %v", err)
	}
	if err := ops.Push(context.Background(), a); err != nil {
		t.Fatalf("push after resolution: %v", err)
	}
	if got := run(t, bare, "show", "main:shared.txt"); !strings.Contains(got, "a and b") {
		t.Errorf("origin should have the resolved content, got %q", got)
	}
}

func TestPush_ParkOnRescueBranch(t *testing.T) {
	bare, a, b := twoClones(t)
	commitFile(t, b, "shared.txt", "b\n", "b")
	run(t, b, "push", "origin", "HEAD")
	commitFile(t, a, "shared.txt", "a\n", "a")

	ops := NewOperations(3, 0)
	if err := ops.Push(context.Background(), a); err == nil {
		t.Fatal("expected conflict")
	}
	if err := ops.AbortRebase(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := ops.PushToBranch(context.Background(), a, "agent-rescue/s1"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, bare, "show", "agent-rescue/s1:shared.txt"); got != "a" {
		t.Errorf("rescue branch should carry the local work, got %q", got)
	}
	if got := run(t, bare, "show", "main:shared.txt"); got != "b" {
		t.Errorf("main should be untouched, got %q", got)
	}
}

func TestPush_RebasesOntoSameNamedBranchNotDefault(t *testing.T) {
	bare, a, b := twoClones(t)
	// Both clones work on "feature"; main moves separately and must not be
	// what we rebase onto.
	run(t, a, "checkout", "-b", "feature")
	run(t, a, "push", "-u", "origin", "feature")
	run(t, b, "fetch", "origin")
	run(t, b, "checkout", "-t", "origin/feature")
	commitFile(t, b, "f-b.txt", "b\n", "feature: b")
	run(t, b, "push", "origin", "HEAD")
	run(t, b, "checkout", "main")
	commitFile(t, b, "main-only.txt", "m\n", "main: moved")
	run(t, b, "push", "origin", "HEAD")

	commitFile(t, a, "f-a.txt", "a\n", "feature: a")
	if err := NewOperations(3, 0).Push(context.Background(), a); err != nil {
		t.Fatalf("push: %v", err)
	}
	files := run(t, bare, "ls-tree", "--name-only", "feature")
	if !strings.Contains(files, "f-a.txt") || !strings.Contains(files, "f-b.txt") {
		t.Errorf("feature should have both feature commits: %s", files)
	}
	if strings.Contains(files, "main-only.txt") {
		t.Errorf("feature must not have been rebased onto main: %s", files)
	}
}
