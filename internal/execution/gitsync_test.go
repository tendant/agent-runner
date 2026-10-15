package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
	gitpkg "github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/repos"
	"github.com/agent-runner/agent-runner/internal/subagent"
)

// funcExecutor runs fn in place of the CLI.
type funcExecutor struct {
	fn func(workspace string) error
}

func (f funcExecutor) Execute(_ context.Context, w, _ string) (*executor.ExecutionResult, error) {
	if err := f.fn(w); err != nil {
		return nil, err
	}
	return &executor.ExecutionResult{Output: "done"}, nil
}
func (f funcExecutor) ExecuteWithSystemPrompt(ctx context.Context, w, _, in string) (*executor.ExecutionResult, error) {
	return f.Execute(ctx, w, in)
}
func (f funcExecutor) ExecuteWithLog(ctx context.Context, w, in string) (*executor.ExecutionResult, string, error) {
	r, err := f.Execute(ctx, w, in)
	return r, "", err
}
func (f funcExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, w, _, in string) (*executor.ExecutionResult, string, error) {
	r, err := f.Execute(ctx, w, in)
	return r, "", err
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// conflictedWorkspace builds workspace/<repo> cloned from a bare origin,
// with a local commit on shared.txt that conflicts with one another clone
// already pushed. Returns the bare path and the workspace (checkout) path.
func conflictedWorkspace(t *testing.T) (bare, workspace string) {
	t.Helper()
	base := t.TempDir()
	bare = filepath.Join(base, "origin.git")
	workspace = filepath.Join(base, "workspace")
	repo := filepath.Join(workspace, "site")
	other := filepath.Join(base, "other")
	os.MkdirAll(workspace, 0o755)
	git(t, base, "init", "--bare", "-b", "main", bare)
	git(t, base, "clone", bare, repo)
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "T")
	os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "init")
	git(t, repo, "push", "-u", "origin", "HEAD")

	git(t, base, "clone", bare, other)
	git(t, other, "config", "user.email", "t@t")
	git(t, other, "config", "user.name", "T")
	os.WriteFile(filepath.Join(other, "shared.txt"), []byte("theirs\n"), 0o644)
	git(t, other, "add", "-A")
	git(t, other, "commit", "-m", "other session")
	git(t, other, "push", "origin", "HEAD")

	os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("mine\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "this session")
	return bare, workspace
}

func runSync(t *testing.T, env *engineTestEnv, workspace string, fn func(string) error) *agent.Session {
	t.Helper()
	env.handlers.executor = funcExecutor{fn: fn}
	env.handlers.config.Agent.PlannerEnabled = false
	env.handlers.config.GitPushRetries = 2
	env.handlers.config.GitPushRetryDelaySeconds = 0

	registerWorkspaceRepos(t, env, workspace)
	session, err := env.handlers.agentManager.CreateSession("edit shared.txt", nil, "test", "", 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	as := &agentSession{backend: env.handlers.Backend()}
	if err := as.start(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	defer as.Close()
	env.handlers.syncWorkspaceRepos(context.Background(), session.ID, "test", session, workspace, "edit shared.txt",
		time.Now().Add(time.Minute), nil, subagent.NewPromptBuilder("preamble"), as)
	return session
}

func TestSyncWorkspaceRepos_AgentResolvesConflict(t *testing.T) {
	env := setupTestEnv(t)
	bare, workspace := conflictedWorkspace(t)
	repo := filepath.Join(workspace, "site")

	calls := 0
	session := runSync(t, env, workspace, func(string) error {
		calls++
		// The agent does what the prompt tells it: resolve, add, continue.
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

	if calls != 1 {
		t.Errorf("expected one resolution iteration, got %d", calls)
	}
	if got := git(t, bare, "show", "main:shared.txt"); got != "mine+theirs" {
		t.Errorf("origin should have the merged result, got %q", got)
	}
	snap := session.Snapshot()
	if len(snap.Warnings) != 0 {
		t.Errorf("no warnings expected, got %v", snap.Warnings)
	}
	if len(snap.Iterations) != 1 || !snap.Iterations[0].Retry || !strings.Contains(snap.Iterations[0].Prompt, "shared.txt") {
		t.Errorf("resolution iteration should be recorded with the conflict prompt: %+v", snap.Iterations)
	}
	if !strings.Contains(snap.Iterations[0].Prompt, "rebase --continue") {
		t.Error("prompt should carry the resolution steps")
	}
}

func TestSyncWorkspaceRepos_ParksUnresolvedWork(t *testing.T) {
	env := setupTestEnv(t)
	bare, workspace := conflictedWorkspace(t)
	repo := filepath.Join(workspace, "site")

	calls := 0
	session := runSync(t, env, workspace, func(string) error {
		calls++ // agent does nothing useful
		return nil
	})

	if calls != maxConflictResolutions {
		t.Errorf("expected %d resolution attempts, got %d", maxConflictResolutions, calls)
	}
	snap := session.Snapshot()
	rescue := "agent-rescue/" + session.ID
	var sawWarn bool
	for _, w := range snap.Warnings {
		if strings.Contains(w, rescue) && strings.Contains(w, "shared.txt") {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Errorf("expected a rescue-branch warning, got %v", snap.Warnings)
	}
	if got := git(t, bare, "show", rescue+":shared.txt"); got != "mine" {
		t.Errorf("rescue branch should carry this session's work, got %q", got)
	}
	if got := git(t, bare, "show", "main:shared.txt"); got != "theirs" {
		t.Errorf("main must not be overwritten, got %q", got)
	}
	if out := git(t, repo, "status", "--porcelain"); out != "" {
		t.Errorf("repo should be left clean after abort, got %q", out)
	}
}

func TestSyncWorkspaceRepos_NoConflictJustPushes(t *testing.T) {
	env := setupTestEnv(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	workspace := filepath.Join(base, "workspace")
	repo := filepath.Join(workspace, "site")
	os.MkdirAll(workspace, 0o755)
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
	git(t, repo, "commit", "-m", "unpushed")

	calls := 0
	session := runSync(t, env, workspace, func(string) error { calls++; return nil })
	if calls != 0 {
		t.Errorf("no iteration should run without a conflict, got %d", calls)
	}
	if got := git(t, bare, "ls-tree", "--name-only", "main"); !strings.Contains(got, "b.txt") {
		t.Errorf("unpushed commit should be pushed: %s", got)
	}
	if w := session.Snapshot().Warnings; len(w) != 0 {
		t.Errorf("unexpected warnings: %v", w)
	}
}

func TestWorkspaceRepos_Layouts(t *testing.T) {
	base := t.TempDir()
	// Multi-repo layout: workspace/{a,b}, plus _send and .claude ignored.
	for _, d := range []string{"a", "b", "_send", ".claude", "plain"} {
		os.MkdirAll(filepath.Join(base, d), 0o755)
	}
	for _, d := range []string{"a", "b", "_send", ".claude"} {
		os.MkdirAll(filepath.Join(base, d, ".git"), 0o755)
	}
	got := workspaceRepos(base)
	if len(got) != 2 || filepath.Base(got[0]) != "a" || filepath.Base(got[1]) != "b" {
		t.Errorf("multi-repo layout: %v", got)
	}
	// Single-repo layout: the checkout itself.
	os.MkdirAll(filepath.Join(base, ".git"), 0o755)
	if got := workspaceRepos(base); len(got) != 1 || got[0] != base {
		t.Errorf("single-repo layout: %v", got)
	}
	if got := workspaceRepos(""); got != nil {
		t.Errorf("empty path: %v", got)
	}
}

// The conflict instructions end with the rebase: the runner pushes, in
// both modes.
func TestConflictContext(t *testing.T) {
	rc := []*gitpkg.RebaseConflictError{{RepoPath: "/w/app", Branch: "main", Files: []string{"a.go"}}}
	box := buildConflictContext(rc)
	if strings.Contains(box, "git push") || !strings.Contains(box, "the runner pushes") {
		t.Errorf("agents must not be told to push:\n%s", box)
	}
	for _, want := range []string{"git add", "rebase --continue", "a.go"} {
		if !strings.Contains(box, want) {
			t.Errorf("missing %q:\n%s", want, box)
		}
	}
	note := runtimeInstructions(runtimeFacts{Sandboxed: true})
	if !strings.Contains(note, "do not `git push`") || !strings.Contains(note, "agent-runner pushes") {
		t.Errorf("runtime note:\n%s", note)
	}
}

// registerWorkspaceRepos registers each repo in workspace under its folder
// name, with its origin as the URL, pushed: what /repo add would do.
func registerWorkspaceRepos(t *testing.T, env *engineTestEnv, workspace string) {
	t.Helper()
	repos.AllowLocalPaths = true
	if env.handlers.config.ReposFile == "" {
		env.handlers.config.ReposFile = filepath.Join(t.TempDir(), "repos.json")
	}
	reg := repos.New(env.handlers.config.ReposFile)
	for _, r := range workspaceRepos(workspace) {
		out, err := exec.Command("git", "-C", r, "remote", "get-url", "origin").Output()
		if err != nil {
			continue
		}
		if err := reg.Put(repos.Repo{Name: filepath.Base(r), URL: strings.TrimSpace(string(out)), Push: true}); err != nil {
			t.Fatal(err)
		}
	}
}

// Commits on a branch the agent made are pushed to that branch; commits on
// a detached HEAD are kept on agent-rescue/<session>.
func TestSyncPushesNewBranchAndParksDetached(t *testing.T) {
	for _, detached := range []bool{false, true} {
		env := setupTestEnv(t)
		base := t.TempDir()
		bare := filepath.Join(base, "origin.git")
		workspace := filepath.Join(base, "workspace")
		repo := filepath.Join(workspace, "site")
		os.MkdirAll(workspace, 0o755)
		git(t, base, "init", "--bare", "-b", "main", bare)
		git(t, base, "clone", bare, repo)
		git(t, repo, "config", "user.email", "t@t")
		git(t, repo, "config", "user.name", "T")
		git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
		git(t, repo, "push", "-u", "origin", "HEAD")
		if detached {
			git(t, repo, "checkout", "-q", "--detach")
		} else {
			git(t, repo, "checkout", "-q", "-b", "feature")
		}
		os.WriteFile(filepath.Join(repo, "f.txt"), []byte("f\n"), 0o644)
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-q", "-m", "agent work")

		session := runSync(t, env, workspace, func(string) error { return nil })
		branch := "feature"
		if detached {
			branch = "agent-rescue/" + session.ID
		}
		if got := git(t, bare, "ls-tree", "--name-only", branch); !strings.Contains(got, "f.txt") {
			t.Errorf("detached=%v: commit not on %s: %q (warnings %v)", detached, branch, got, session.Snapshot().Warnings)
		}
	}
}
