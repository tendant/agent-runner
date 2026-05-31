package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
)

func TestBuildErrorContext_Basic(t *testing.T) {
	result := buildErrorContext(3, "timeout exceeded", "some output here")

	if !strings.Contains(result, "iteration 3") {
		t.Error("expected iteration number in output")
	}
	if !strings.Contains(result, "timeout exceeded") {
		t.Error("expected error message in output")
	}
	if !strings.Contains(result, "some output here") {
		t.Error("expected partial output in output")
	}
}

func TestBuildErrorContext_EmptyPartialOutput(t *testing.T) {
	result := buildErrorContext(1, "failed", "")

	if !strings.Contains(result, "failed") {
		t.Error("expected error message")
	}
	if strings.Contains(result, "Partial output") {
		t.Error("should not include partial output section when empty")
	}
}

func TestBuildErrorContext_TruncatesLongOutput(t *testing.T) {
	longOutput := strings.Repeat("x", 3000)
	result := buildErrorContext(1, "err", longOutput)

	if !strings.Contains(result, "... (truncated)") {
		t.Error("expected truncation marker")
	}
	// The partial output in the result should be capped at maxPartialOutputChars
	if strings.Contains(result, strings.Repeat("x", 2001)) {
		t.Error("output should be truncated to 2000 chars")
	}
}

func TestBuildErrorContext_ExactlyAtLimit(t *testing.T) {
	exactOutput := strings.Repeat("y", maxPartialOutputChars)
	result := buildErrorContext(1, "err", exactOutput)

	if strings.Contains(result, "... (truncated)") {
		t.Error("output at exactly the limit should not be truncated")
	}
	if !strings.Contains(result, exactOutput) {
		t.Error("expected full output when at limit")
	}
}

func TestCollectOutputFiles_NonexistentDir(t *testing.T) {
	files, err := collectOutputFiles("/nonexistent/path")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir, got: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil files, got %d", len(files))
	}
}

func TestCollectOutputFiles_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestCollectOutputFiles_CollectsFiles(t *testing.T) {
	dir := t.TempDir()

	// Create test files
	os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("world"), 0644)

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Check that file data is correct
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
		if f.ContentType == "" {
			t.Errorf("file %s has empty content type", f.Name)
		}
	}
	if !names["file1.txt"] || !names["file2.txt"] {
		t.Error("expected both file1.txt and file2.txt")
	}
}

func TestCollectOutputFiles_SkipsDirs(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file (dirs skipped), got %d", len(files))
	}
	if files[0].Name != "file.txt" {
		t.Errorf("expected file.txt, got %s", files[0].Name)
	}
}

func TestCollectOutputFiles_FileLimit(t *testing.T) {
	dir := t.TempDir()

	// Create 25 files (exceeds maxOutputFiles = 20)
	for i := 0; i < 25; i++ {
		name := filepath.Join(dir, strings.Replace("file_XX.txt", "XX", strings.Repeat("a", i+1), 1))
		os.WriteFile(name, []byte("data"), 0644)
	}

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) > maxOutputFiles {
		t.Errorf("expected at most %d files, got %d", maxOutputFiles, len(files))
	}
}

func TestCollectOutputFiles_SizeLimit(t *testing.T) {
	dir := t.TempDir()

	// Create a file that's just under the size limit
	bigData := make([]byte, 9<<20) // 9MB
	os.WriteFile(filepath.Join(dir, "big.bin"), bigData, 0644)

	// Create another file that would exceed the limit
	os.WriteFile(filepath.Join(dir, "extra.bin"), make([]byte, 2<<20), 0644) // 2MB

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should collect the big file but skip the extra one (9MB + 2MB > 10MB)
	if len(files) != 1 {
		t.Errorf("expected 1 file (size limit), got %d", len(files))
	}
}

// --- pushUnpushedCommits ---

func initBareAndClone(t *testing.T) (bareDir, workDir string) {
	t.Helper()
	base := t.TempDir()
	bareDir = filepath.Join(base, "origin.git")
	workDir = filepath.Join(base, "work")
	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, "", "clone", bareDir, workDir)
	mustGit(t, workDir, "config", "user.email", "test@test.com")
	mustGit(t, workDir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(workDir, "init.txt"), []byte("init"), 0644)
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "initial")
	mustGit(t, workDir, "push", "origin", "HEAD")
	return
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestPushUnpushedCommits_NothingToPush(t *testing.T) {
	_, workDir := initBareAndClone(t)
	warn := pushUnpushedCommits(context.Background(), workDir, 1, 0)
	if warn != "" {
		t.Errorf("expected no warning when repo is clean, got: %s", warn)
	}
}

func TestPushUnpushedCommits_PushesAndReturnsNoWarn(t *testing.T) {
	_, workDir := initBareAndClone(t)

	// Commit locally without pushing.
	os.WriteFile(filepath.Join(workDir, "new.txt"), []byte("new"), 0644)
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "unpushed commit")

	warn := pushUnpushedCommits(context.Background(), workDir, 1, 0)
	if warn != "" {
		t.Errorf("expected push to succeed and return no warning, got: %s", warn)
	}
}

func TestPushUnpushedCommits_ReturnsWarnOnPushFailure(t *testing.T) {
	_, workDir := initBareAndClone(t)

	// Commit locally without pushing.
	os.WriteFile(filepath.Join(workDir, "new.txt"), []byte("new"), 0644)
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "unpushed commit")

	// Break the remote URL so push fails.
	mustGit(t, workDir, "remote", "set-url", "origin", "/nonexistent/path.git")

	warn := pushUnpushedCommits(context.Background(), workDir, 1, 0)
	if warn == "" {
		t.Error("expected a warning when push fails, got empty string")
	}
	if !strings.Contains(warn, "git push failed") {
		t.Errorf("expected 'git push failed' in warning, got: %s", warn)
	}
}

// --- BootstrapWarnings ---

func TestBootstrapWarnings_ClaudeNoKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	warns := BootstrapWarnings("claude", "")
	if len(warns) == 0 {
		t.Error("expected warning when ANTHROPIC_API_KEY is missing for claude")
	}
}

func TestBootstrapWarnings_ClaudeWithKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	warns := BootstrapWarnings("claude", "")
	if len(warns) != 0 {
		t.Errorf("expected no warnings with ANTHROPIC_API_KEY set, got: %v", warns)
	}
}

func TestBootstrapWarnings_OpencodeNoKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	warns := BootstrapWarnings("opencode", "")
	if len(warns) == 0 {
		t.Error("expected warning when no API key is set for opencode")
	}
}

func TestBootstrapWarnings_OpencodeWithProviderKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	warns := BootstrapWarnings("opencode", "deepseek")
	if len(warns) != 0 {
		t.Errorf("expected no warnings with DEEPSEEK_API_KEY set, got: %v", warns)
	}
}

// --- Panic recovery ---

// panicExecutor implements executor.Executor and panics on every call.
type panicExecutor struct{}

func (panicExecutor) Execute(_ context.Context, _, _ string) (*executor.ExecutionResult, error) {
	panic("injected panic")
}
func (panicExecutor) ExecuteWithSystemPrompt(_ context.Context, _, _, _ string) (*executor.ExecutionResult, error) {
	panic("injected panic")
}
func (panicExecutor) ExecuteWithLog(_ context.Context, _, _ string) (*executor.ExecutionResult, string, error) {
	panic("injected panic")
}
func (panicExecutor) ExecuteWithLogAndSystemPrompt(_ context.Context, _, _, _ string) (*executor.ExecutionResult, string, error) {
	panic("injected panic")
}

func TestPanicRecovery_MarksSessionFailed(t *testing.T) {
	env := setupTestEnv(t)
	// Inject the panic executor.
	env.handlers.executor = panicExecutor{}

	session, err := env.handlers.agentManager.CreateSession("do something", nil, "test", "", 1, 10)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Run synchronously so we can inspect state immediately.
	env.handlers.executeAgent(session)

	snap := session.Snapshot()
	if snap.Status != agent.SessionStatusFailed {
		t.Errorf("expected session status=failed after panic, got %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "panic") {
		t.Errorf("expected 'panic' in error message, got: %s", snap.Error)
	}
}

// Ensure panicExecutor satisfies the interface at compile time.
var _ executor.Executor = panicExecutor{}
var _ = fmt.Sprintf // suppress unused import
