package subagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptBuilder_BuildStatic(t *testing.T) {
	pb := NewPromptBuilder("You are a helpful assistant.\n\nTask: Do something")
	result := pb.BuildStatic("ignored message", "")

	if result != "You are a helpful assistant.\n\nTask: Do something" {
		t.Errorf("expected preamble unchanged, got '%s'", result)
	}
}

func TestPromptBuilder_Build_WithPlanNoPreamble(t *testing.T) {
	dir := t.TempDir()

	pb := NewPromptBuilder("Base prompt here")
	plan := &PlanResult{
		Summary:  "Fix the login bug",
		Approach: "Modify auth handler",
		Steps: []PlanStep{
			{ID: "1", Description: "Find the bug", Done: true},
			{ID: "2", Description: "Fix the handler", Files: []string{"auth.go"}, Done: false},
			{ID: "3", Description: "Add tests", Done: false},
		},
	}

	result := pb.Build(context.Background(), dir, plan, 2, "fix login", "")

	if !strings.Contains(result, "Base prompt here") {
		t.Error("expected preamble in output")
	}
	if !strings.Contains(result, "## Plan") {
		t.Error("expected plan section")
	}
	if !strings.Contains(result, "[x] 1: Find the bug") {
		t.Error("expected checked step 1")
	}
	if !strings.Contains(result, "[ ] 2: Fix the handler (auth.go)") {
		t.Error("expected unchecked step 2 with files")
	}
	if !strings.Contains(result, "**Iteration:** 2") {
		t.Error("expected iteration number")
	}
	if !strings.Contains(result, "**Goal:** Fix the login bug") {
		t.Error("expected summary")
	}
}

func TestPromptBuilder_Build_NilPlan(t *testing.T) {
	dir := t.TempDir()

	pb := NewPromptBuilder("Do the work")
	result := pb.Build(context.Background(), dir, nil, 1, "msg", "")

	if !strings.Contains(result, "Do the work") {
		t.Error("expected preamble in output")
	}
	if strings.Contains(result, "## Plan") {
		t.Error("did not expect plan section with nil plan")
	}
	if !strings.Contains(result, "**Iteration:** 1") {
		t.Error("expected iteration number")
	}
}

func TestPromptBuilder_Build_WithTodo(t *testing.T) {
	dir := t.TempDir()

	// Create TODO.md
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("- [ ] First task\n- [x] Done task\n"), 0644)

	pb := NewPromptBuilder("preamble")
	result := pb.Build(context.Background(), dir, nil, 1, "msg", "")

	if !strings.Contains(result, "## Current TODO.md") {
		t.Error("expected TODO section")
	}
	if !strings.Contains(result, "First task") {
		t.Error("expected TODO content")
	}
}

func TestPromptBuilder_Build_WithGitRepo(t *testing.T) {
	dir := t.TempDir()

	// Create a repo with a commit
	repoDir := filepath.Join(dir, "my-app")
	os.MkdirAll(repoDir, 0755)

	cmd := exec.Command("git", "init")
	cmd.Dir = repoDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = repoDir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.name", "Test")
	cmd.Dir = repoDir
	cmd.Run()
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("hello"), 0644)
	cmd = exec.Command("git", "add", "-A")
	cmd.Dir = repoDir
	cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "Initial commit")
	cmd.Dir = repoDir
	cmd.Run()

	pb := NewPromptBuilder("preamble")
	result := pb.Build(context.Background(), dir, nil, 1, "msg", "")

	if !strings.Contains(result, "## Recent Commits") {
		t.Error("expected commits section")
	}
	if !strings.Contains(result, "my-app: ") {
		t.Error("expected commit with repo prefix")
	}
}

func TestPromptBuilder_Build_WithErrorContext(t *testing.T) {
	dir := t.TempDir()

	pb := NewPromptBuilder("preamble")
	errCtx := "## Previous Iteration Error (iteration 2)\n\n**Error:** claude execution failed: timeout\n\n**Partial output:**\n```\nsome partial work\n```\n"
	result := pb.Build(context.Background(), dir, nil, 3, "msg", errCtx)

	if !strings.Contains(result, "## Previous Iteration Error (iteration 2)") {
		t.Error("expected error context section")
	}
	if !strings.Contains(result, "claude execution failed: timeout") {
		t.Error("expected error message in context")
	}
	if !strings.Contains(result, "some partial work") {
		t.Error("expected partial output in context")
	}
	if !strings.Contains(result, "**Iteration:** 3") {
		t.Error("expected iteration number after error context")
	}
}

func TestPromptBuilder_Build_WithProgressFile(t *testing.T) {
	dir := t.TempDir()

	// Write a progress file marking steps 1 and 3 as completed
	os.WriteFile(filepath.Join(dir, "_progress.json"), []byte(`{"completed_steps":["1","3"]}`), 0644)

	pb := NewPromptBuilder("preamble")
	plan := &PlanResult{
		Summary: "Deploy app",
		Steps: []PlanStep{
			{ID: "1", Description: "Clone repo", Done: false},
			{ID: "2", Description: "Build image", Done: false},
			{ID: "3", Description: "Push image", Done: false},
		},
	}

	result := pb.Build(context.Background(), dir, plan, 3, "deploy", "")

	if !strings.Contains(result, "[x] 1: Clone repo") {
		t.Error("expected step 1 checked via progress file")
	}
	if !strings.Contains(result, "[ ] 2: Build image") {
		t.Error("expected step 2 unchecked")
	}
	if !strings.Contains(result, "[x] 3: Push image") {
		t.Error("expected step 3 checked via progress file")
	}
}

func TestPromptBuilder_BuildStatic_WithErrorContext(t *testing.T) {
	pb := NewPromptBuilder("base prompt")
	errCtx := "## Previous Iteration Error (iteration 1)\n\n**Error:** something broke\n"
	result := pb.BuildStatic("msg", errCtx)

	if !strings.Contains(result, "base prompt") {
		t.Error("expected preamble")
	}
	if !strings.Contains(result, "## Previous Iteration Error") {
		t.Error("expected error context appended")
	}
}
