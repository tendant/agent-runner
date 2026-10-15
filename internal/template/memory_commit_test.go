package template

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func memRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("d1\n"), 0o644)
	if _, err := CommitMemory(dir, "init"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCommitMemoryAndLog(t *testing.T) {
	dir := memRepo(t)
	if ok, err := CommitMemory(dir, "nothing"); ok || err != nil {
		t.Errorf("nothing changed: %v %v", ok, err)
	}
	os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("d1\nd2\n"), 0o644)
	msg := SessionCommitMessage("agent-1", "stream", []string{"decisions.md"})
	if ok, err := CommitMemory(dir, msg); !ok || err != nil {
		t.Fatalf("commit: %v %v", ok, err)
	}
	log, err := MemoryLog(dir, 10)
	if err != nil || len(log) != 2 || !strings.Contains(log[0], "memory: session agent-1 (stream): saved decisions.md") {
		t.Errorf("log %q %v", log, err)
	}
	if ok, _ := CommitMemory(t.TempDir(), "x"); ok {
		t.Error("not a repo: nothing to commit")
	}
}

func TestUndoMemorySession(t *testing.T) {
	dir := memRepo(t)
	write := func(name, body, msg string) {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		if _, err := CommitMemory(dir, msg); err != nil {
			t.Fatal(err)
		}
	}
	write("decisions.md", "d1\nfrom-1\n", SessionCommitMessage("agent-1", "stream", []string{"decisions.md"}))
	write("notes.md", "n\n", SessionCommitMessage("agent-10", "stream", []string{"notes.md"})) // a prefix of agent-1? no: agent-1 vs agent-10
	write("2026-10-15-h.md", "log\n", "memory: daily log (session agent-1)")

	got, err := UndoMemorySession(dir, "agent-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("undo: %v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "decisions.md")); string(b) != "d1\n" {
		t.Errorf("agent-1's note not undone: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); err != nil {
		t.Error("agent-10's note must stay")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-10-15-h.md")); err != nil {
		t.Error("the daily log must stay")
	}
	if _, err := UndoMemorySession(dir, "agent-404"); err == nil {
		t.Error("unknown session: want an error")
	}
	if _, err := UndoMemorySession(dir, "x --all"); err == nil {
		t.Error("a bad id must be refused")
	}
}

// A later edit to the same lines: nothing is undone, and the tree is as it was.
func TestUndoMemorySessionConflict(t *testing.T) {
	dir := memRepo(t)
	os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("d1\nfrom-1\n"), 0o644)
	CommitMemory(dir, SessionCommitMessage("agent-1", "stream", []string{"decisions.md"}))
	os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("d1\nfrom-1 edited\n"), 0o644)
	CommitMemory(dir, SessionCommitMessage("agent-2", "stream", []string{"decisions.md"}))

	if _, err := UndoMemorySession(dir, "agent-1"); err == nil || !strings.Contains(err.Error(), "nothing was undone") {
		t.Fatalf("want a conflict, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "decisions.md")); string(b) != "d1\nfrom-1 edited\n" {
		t.Errorf("tree changed: %q", b)
	}
	out, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if len(out) != 0 {
		t.Errorf("repo left dirty: %s", out)
	}
}

// A memory dir inside some other repo (DATA_DIR defaults to the current
// folder) is not committed there unless MEMORY_GIT_PARENT_REPO asks.
func TestCommitAndPushMemoryParentRepoOptIn(t *testing.T) {
	outer := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"}} {
		exec.Command("git", append([]string{"-C", outer}, args...)...).Run()
	}
	mem := filepath.Join(outer, "memory")
	os.MkdirAll(mem, 0o755)
	os.WriteFile(filepath.Join(mem, "decisions.md"), []byte("d\n"), 0o644)
	count := func() string {
		out, _ := exec.Command("git", "-C", outer, "rev-list", "--all", "--count").Output()
		return strings.TrimSpace(string(out))
	}
	t.Setenv("MEMORY_GIT_PARENT_REPO", "")
	if err := CommitAndPushMemory(mem, MemoryGitCreds{}); err != nil || count() != "0" {
		t.Errorf("committed into the outer repo unasked: %v, %s commits", err, count())
	}
	t.Setenv("MEMORY_GIT_PARENT_REPO", "true")
	if err := CommitAndPushMemory(mem, MemoryGitCreds{}); err != nil || count() != "1" {
		t.Errorf("opt-in: %v, %s commits", err, count())
	}
}

// Pulling into a memory repo with no commits yet (after /memory git on an
// empty dir) must not panic.
func TestPullMemoryIntoEmptyRepo(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	exec.Command("git", "init", "-q", "--bare", "-b", "main", remote).Run()
	seed := t.TempDir()
	for _, args := range [][]string{{"clone", "-q", remote, seed}, {"-C", seed, "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "x"}, {"-C", seed, "push", "-q", "origin", "HEAD:main"}} {
		exec.Command("git", args...).Run()
	}
	mem := t.TempDir()
	exec.Command("git", "-C", mem, "init", "-q", "-b", "main").Run()
	exec.Command("git", "-C", mem, "remote", "add", "origin", remote).Run()
	if _, err := PullMemory(mem, MemoryGitCreds{}); err != nil {
		t.Logf("pull: %v", err) // an error is fine; a panic is not
	}
	if shortHash(nil) != "(none)" || shortHash([]byte("0123456789abc\n")) != "01234567" {
		t.Error("shortHash")
	}
}

// Two sessions merging at once each get a commit of their own files only,
// and an operator's uncommitted edit is in neither.
func TestMergeCommitsOnlyOwnFiles(t *testing.T) {
	dir := memRepo(t)
	a, _ := StageMemory(dir, filepath.Join(t.TempDir(), "a"))
	b, _ := StageMemory(dir, filepath.Join(t.TempDir(), "b"))
	os.WriteFile(filepath.Join(a.Dir, "alpha.md"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(b.Dir, "beta.md"), []byte("b\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "agent.md"), []byte("operator edit\n"), 0o644) // not committed
	done := make(chan struct{})
	go func() {
		a.MergeAndCommit(func(s []string) string { return SessionCommitMessage("A", "t", s) })
		close(done)
	}()
	b.MergeAndCommit(func(s []string) string { return SessionCommitMessage("B", "t", s) })
	<-done
	files := func(subject string) string {
		out, _ := exec.Command("git", "-C", dir, "log", "--format=", "--name-only", "--grep", subject).Output()
		return strings.TrimSpace(string(out))
	}
	if got := files("session A "); got != "alpha.md" {
		t.Errorf("A's commit: %q", got)
	}
	if got := files("session B "); got != "beta.md" {
		t.Errorf("B's commit: %q", got)
	}
	if out, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); !strings.Contains(string(out), "agent.md") {
		t.Error("the operator's edit must stay uncommitted, not swept into a session's commit")
	}
}

// A session's append is undone even after a later session appended to the
// same file; undoing it twice says so.
func TestUndoAppendAfterLaterAppend(t *testing.T) {
	dir := memRepo(t) // decisions.md = "d1\n"
	write := func(body, msg string) {
		os.WriteFile(filepath.Join(dir, "decisions.md"), []byte(body), 0o644)
		CommitMemoryFiles(dir, msg, "decisions.md")
	}
	write("d1\nfrom-1\n", SessionCommitMessage("agent-1", "t", []string{"decisions.md"}))
	write("d1\nfrom-1\nfrom-2\n", SessionCommitMessage("agent-2", "t", []string{"decisions.md"}))
	if _, err := UndoMemorySession(dir, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "decisions.md")); string(b) != "d1\nfrom-2\n" {
		t.Errorf("after undo: %q", b)
	}
	if _, err := UndoMemorySession(dir, "agent-1"); err == nil || !strings.Contains(err.Error(), "already undone") {
		t.Errorf("second undo: %v", err)
	}
}

// An operator's edit to agent.md is its own commit.
func TestWriteInstructionsCommitsAlone(t *testing.T) {
	dir := memRepo(t)
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("left over\n"), 0o644)
	if err := WriteInstructions(dir, filepath.Join(dir, "agent.md"), "be kind\n", "memory: agent.md edited by the operator (chat)"); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("git", "-C", dir, "show", "--name-only", "--format=%s", "HEAD").Output()
	if got := strings.TrimSpace(string(out)); got != "memory: agent.md edited by the operator (chat)\n\nagent.md" {
		t.Errorf("HEAD: %q", got)
	}
}

// Committing a file that never existed (lessons.md before the first lesson)
// commits what does exist instead of failing.
func TestCommitMemoryFilesSkipsMissing(t *testing.T) {
	dir := memRepo(t)
	os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("d1\nd2\n"), 0o644)
	ok, err := CommitMemoryFiles(dir, "memory: curation (session s)", "lessons.md", "decisions.md")
	if err != nil || !ok {
		t.Fatalf("commit: %v %v", ok, err)
	}
	if ok, err := CommitMemoryFiles(dir, "nothing", "lessons.md"); ok || err != nil {
		t.Errorf("only a missing file: %v %v", ok, err)
	}
}

// The session's own append is removed, not a later identical one.
func TestUndoRemovesTheSessionsOwnAppend(t *testing.T) {
	dir := memRepo(t) // decisions.md = "d1\n"
	write := func(body, msg string) {
		os.WriteFile(filepath.Join(dir, "decisions.md"), []byte(body), 0o644)
		CommitMemoryFiles(dir, msg, "decisions.md")
	}
	write("d1\n- a\n", SessionCommitMessage("s1", "t", []string{"decisions.md"}))
	write("d1\n- a\n- b\n- a\n", SessionCommitMessage("s2", "t", []string{"decisions.md"}))
	if _, err := UndoMemorySession(dir, "s1"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "decisions.md")); string(b) != "d1\n- b\n- a\n" {
		t.Errorf("after undo: %q", b)
	}
}
