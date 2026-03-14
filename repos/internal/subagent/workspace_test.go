package subagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadWorkspaceState_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	state := ReadWorkspaceState(context.Background(), dir)

	if state.TodoContent != "" {
		t.Errorf("expected empty TodoContent, got '%s'", state.TodoContent)
	}
	if len(state.RepoNames) != 0 {
		t.Errorf("expected no repos, got %v", state.RepoNames)
	}
	if len(state.RecentCommits) != 0 {
		t.Errorf("expected no commits, got %v", state.RecentCommits)
	}
}

func TestReadWorkspaceState_WithTodoAndRepo(t *testing.T) {
	dir := t.TempDir()

	// Create TODO.md at workspace root
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("- [ ] Fix bug\n- [x] Add tests\n"), 0644)

	// Create a git repo in the workspace
	repoDir := filepath.Join(dir, "my-repo")
	os.MkdirAll(repoDir, 0755)
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "test@test.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("hello"), 0644)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "Initial commit")

	state := ReadWorkspaceState(context.Background(), dir)

	if !strings.Contains(state.TodoContent, "Fix bug") {
		t.Errorf("expected TodoContent to contain 'Fix bug', got '%s'", state.TodoContent)
	}

	if len(state.RepoNames) != 1 || state.RepoNames[0] != "my-repo" {
		t.Errorf("expected [my-repo], got %v", state.RepoNames)
	}

	if len(state.RecentCommits) == 0 {
		t.Error("expected at least one commit")
	}
	if !strings.Contains(state.RecentCommits[0], "my-repo: ") {
		t.Errorf("expected commit prefixed with repo name, got '%s'", state.RecentCommits[0])
	}
}

func TestReadWorkspaceState_WithUncommittedChanges(t *testing.T) {
	dir := t.TempDir()

	repoDir := filepath.Join(dir, "repo")
	os.MkdirAll(repoDir, 0755)
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "test@test.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("v1"), 0644)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "v1")

	// Make uncommitted changes
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("v2 changed"), 0644)

	state := ReadWorkspaceState(context.Background(), dir)

	if state.GitDiffStat == "" {
		t.Error("expected non-empty GitDiffStat for uncommitted changes")
	}
	if !strings.Contains(state.GitDiffStat, "file.txt") {
		t.Errorf("expected diff stat to mention file.txt, got '%s'", state.GitDiffStat)
	}
}

func TestReadWorkspaceState_SkipsDotAndUnderscoreDirs(t *testing.T) {
	dir := t.TempDir()

	// .hidden and _send should be skipped; only git repos are listed
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0755)
	os.MkdirAll(filepath.Join(dir, "_send"), 0755)
	visibleDir := filepath.Join(dir, "visible")
	os.MkdirAll(visibleDir, 0755)
	runGit(t, visibleDir, "init")

	state := ReadWorkspaceState(context.Background(), dir)

	if len(state.RepoNames) != 1 || state.RepoNames[0] != "visible" {
		t.Errorf("expected [visible], got %v", state.RepoNames)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
