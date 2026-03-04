package template

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegration_DefaultsOnlyPrompt verifies that with no config at all,
// embedded defaults produce a sensible composed prompt.
func TestIntegration_DefaultsOnlyPrompt(t *testing.T) {
	ctx := NewContext("Build a landing page", []string{"site-abc"}, 1)

	result, err := ComposePrompt("", PhaseBoot, false, ctx)
	if err != nil {
		t.Fatalf("ComposePrompt error: %v", err)
	}

	t.Logf("=== Defaults-only prompt (%d chars) ===\n%s", len(result), result)

	// Should include always + boot templates (not heartbeat, not first_run)
	assertContains(t, result, "autonomous", "should contain IDENTITY content")
	assertContains(t, result, "Behavioral Guidelines", "should contain SOUL content")
	assertContains(t, result, "Agent Collaboration", "should contain AGENTS content")
	assertContains(t, result, "User Context", "should contain USER content")
	assertContains(t, result, "Tools and Conventions", "should contain TOOLS content")
	assertContains(t, result, "Session Start", "should contain BOOT content")
	assertNotContains(t, result, "Heartbeat", "should NOT contain HEARTBEAT in boot phase")

	// Variable substitution
	assertContains(t, result, time.Now().Format("2006-01-02"), "should substitute {{DATE}}")
}

// TestIntegration_UserOverride verifies user templates override embedded defaults.
func TestIntegration_UserOverride(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(`---
title: Custom Soul
read_when: always
priority: 20
---

# Custom Behavior

Always write tests first. Use TDD.`), 0644)

	ctx := NewContext("Fix the login bug", []string{}, 1)
	result, err := ComposePrompt(dir, PhaseBoot, false, ctx)
	if err != nil {
		t.Fatalf("ComposePrompt error: %v", err)
	}

	t.Logf("=== Override prompt (%d chars) ===\n%s", len(result), result)

	assertContains(t, result, "Always write tests first", "user SOUL.md should override default")
	assertNotContains(t, result, "Work methodically", "default SOUL content should be replaced")
	assertContains(t, result, "autonomous", "other defaults should still be present")
}

// TestIntegration_ExtraUserTemplate verifies user can add custom templates.
func TestIntegration_ExtraUserTemplate(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "STACK.md"), []byte(`---
title: Tech Stack
read_when: always
priority: 45
---

Use Next.js 14 with Tailwind CSS and TypeScript.`), 0644)

	ctx := NewContext("Create a dashboard", []string{}, 1)
	result, err := ComposePrompt(dir, PhaseBoot, false, ctx)
	if err != nil {
		t.Fatalf("ComposePrompt error: %v", err)
	}

	t.Logf("=== Extra template prompt (%d chars) ===\n%s", len(result), result)

	assertContains(t, result, "Next.js 14 with Tailwind", "custom STACK.md should be included")
	// Verify ordering: USER (40) < STACK (45) < TOOLS (50)
	idxUser := strings.Index(result, "User Context")
	idxStack := strings.Index(result, "Tech Stack")
	idxTools := strings.Index(result, "Tools and Conventions")
	if idxUser < 0 || idxStack < 0 || idxTools < 0 {
		t.Fatal("missing expected sections")
	}
	if !(idxUser < idxStack && idxStack < idxTools) {
		t.Errorf("priority ordering wrong: USER(%d) < STACK(%d) < TOOLS(%d)", idxUser, idxStack, idxTools)
	}
}

// TestIntegration_Bootstrap verifies first-run includes BOOTSTRAP.md.
func TestIntegration_Bootstrap(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "BOOTSTRAP.md"), []byte(`---
title: First Run Setup
read_when: first_run
priority: 80
---

Welcome! This is your first session. Please read README.md and set up the project.`), 0644)

	if !IsFirstRun(dir) {
		t.Fatal("IsFirstRun should be true")
	}

	ctx := NewContext("Initialize project", []string{}, 1)
	result, err := ComposePrompt(dir, PhaseBoot, true, ctx)
	if err != nil {
		t.Fatalf("ComposePrompt error: %v", err)
	}

	t.Logf("=== Bootstrap prompt (%d chars) ===\n%s", len(result), result)
	assertContains(t, result, "first session", "BOOTSTRAP.md should be included on first run")

	// Complete bootstrap
	if err := CompleteBootstrap(dir); err != nil {
		t.Fatalf("CompleteBootstrap error: %v", err)
	}
	if IsFirstRun(dir) {
		t.Error("IsFirstRun should be false after completion")
	}

	// Verify .done file exists
	if _, err := os.Stat(filepath.Join(dir, "BOOTSTRAP.md.done")); err != nil {
		t.Error("BOOTSTRAP.md.done should exist")
	}
}

// TestIntegration_Memory verifies memory section composition.
func TestIntegration_Memory(t *testing.T) {
	memDir := t.TempDir()

	// Curated memory (now lives in memory dir after seeding)
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(`# Project Notes

- The API uses JWT auth
- Database is PostgreSQL 16
- Deploy via GitHub Actions`), 0644)

	// Daily logs
	os.WriteFile(filepath.Join(memDir, "2026-03-01.md"), []byte("Set up CI pipeline"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-02.md"), []byte("Fixed auth middleware"), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-03-03.md"), []byte("Added user dashboard"), 0644)

	result := ComposeMemorySection(memDir, 7)
	t.Logf("=== Memory section ===\n%s", result)

	assertContains(t, result, "## Memory", "should have Memory header")
	assertContains(t, result, "JWT auth", "should contain curated memory")
	assertContains(t, result, "Set up CI pipeline", "should contain daily log")
	assertContains(t, result, "Added user dashboard", "should contain today's log")

	// Test with days=2 limit
	result2 := ComposeMemorySection(memDir, 2)
	assertNotContains(t, result2, "Set up CI pipeline", "oldest log should be excluded with days=2")
	assertContains(t, result2, "Fixed auth middleware", "second day should be included")
	assertContains(t, result2, "Added user dashboard", "today should be included")
}

// TestIntegration_HeartbeatPhase verifies heartbeat phase filtering.
func TestIntegration_HeartbeatPhase(t *testing.T) {
	ctx := NewContext("ongoing task", []string{}, 5)

	result, err := ComposePrompt("", PhaseHeartbeat, false, ctx)
	if err != nil {
		t.Fatalf("ComposePrompt error: %v", err)
	}

	t.Logf("=== Heartbeat prompt (%d chars) ===\n%s", len(result), result)

	assertContains(t, result, "autonomous", "should contain always-loaded IDENTITY")
	assertContains(t, result, "status update", "should contain HEARTBEAT content")
	assertNotContains(t, result, "Session Start", "should NOT contain BOOT content in heartbeat phase")
}

// TestIntegration_HeartbeatConfig verifies heartbeat config parsing.
func TestIntegration_HeartbeatConfig(t *testing.T) {
	// Default from embedded
	cfg := ParseHeartbeatConfig("")
	if cfg.IntervalSeconds != 300 {
		t.Errorf("default interval = %d, want 300", cfg.IntervalSeconds)
	}
	if cfg.Prompt == "" {
		t.Error("should have default prompt")
	}
	t.Logf("Default heartbeat: interval=%ds prompt=%q", cfg.IntervalSeconds, cfg.Prompt)

	// User override
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte(`---
title: Fast Heartbeat
read_when: heartbeat
priority: 90
interval_seconds: 60
---

Quick status check.`), 0644)

	cfg2 := ParseHeartbeatConfig(dir)
	if cfg2.IntervalSeconds != 60 {
		t.Errorf("override interval = %d, want 60", cfg2.IntervalSeconds)
	}
	assertContains(t, cfg2.Prompt, "Quick status check", "should use override prompt")
	t.Logf("Override heartbeat: interval=%ds prompt=%q", cfg2.IntervalSeconds, cfg2.Prompt)
}

// TestIntegration_VariableSubstitution verifies all template variables work.
func TestIntegration_VariableSubstitution(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(`---
title: Task Info
read_when: always
priority: 99
---

Task: {{MESSAGE}}
Repos: {{REPOS}}
Date: {{DATE}}
Iteration: {{ITERATION}}`), 0644)

	ctx := TemplateContext{
		Message:   "deploy the app",
		Repos:     "frontend, backend",
		Date:      "2026-03-03",
		Iteration: 42,
	}

	result, err := ComposePrompt(dir, PhaseBoot, false, ctx)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	t.Logf("=== Variable substitution ===\n%s", result)

	assertContains(t, result, "Task: deploy the app", "MESSAGE substitution")
	assertContains(t, result, "Repos: frontend, backend", "REPOS substitution")
	assertContains(t, result, "Date: 2026-03-03", "DATE substitution")
	assertContains(t, result, "Iteration: 42", "ITERATION substitution")
}

// TestIntegration_FullPipeline exercises the complete pipeline: defaults + override + extra + memory + bootstrap.
func TestIntegration_FullPipeline(t *testing.T) {
	dir := t.TempDir()

	// Override SOUL
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(`---
title: Project Soul
read_when: always
priority: 20
---

Be precise. Follow the spec exactly.`), 0644)

	// Extra template
	os.WriteFile(filepath.Join(dir, "INFRA.md"), []byte(`---
title: Infrastructure
read_when: always
priority: 55
---

Deploy to Kubernetes via ArgoCD.`), 0644)

	// Bootstrap
	os.WriteFile(filepath.Join(dir, "BOOTSTRAP.md"), []byte(`---
title: Welcome
read_when: first_run
priority: 80
---

First time setup: run npm install.`), 0644)

	// Memory — everything in one dir
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("Key fact: API rate limit is 100/min"), 0644)
	os.WriteFile(filepath.Join(dir, "2026-03-03.md"), []byte("Deployed v2.1"), 0644)

	ctx := NewContext("Add rate limiting", []string{"api-server"}, 1)

	// First run — dir is the memory dir (templates + memory + daily logs)
	result, err := ComposePrompt(dir, PhaseBoot, true, ctx)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	memorySec := ComposeMemorySection(dir, 7)
	if memorySec != "" {
		result += "\n\n" + memorySec
	}

	t.Logf("=== Full pipeline prompt (%d chars) ===\n%s", len(result), result)

	// Verify all pieces
	assertContains(t, result, "autonomous", "IDENTITY default present")
	assertContains(t, result, "Be precise", "SOUL override applied")
	assertNotContains(t, result, "Work methodically", "default SOUL replaced")
	assertContains(t, result, "Agent Collaboration", "AGENTS default present")
	assertContains(t, result, "User Context", "USER default present")
	assertContains(t, result, "Tools and Conventions", "TOOLS default present")
	assertContains(t, result, "Deploy to Kubernetes", "INFRA extra template")
	assertContains(t, result, "Session Start", "BOOT template")
	assertContains(t, result, "npm install", "BOOTSTRAP first_run")
	assertContains(t, result, "rate limit is 100", "MEMORY.md")
	assertContains(t, result, "Deployed v2.1", "daily memory log")

	fmt.Printf("\n✓ Full pipeline: %d chars, all sections present\n", len(result))
}

func assertContains(t *testing.T, s, substr, msg string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("%s: missing %q", msg, substr)
	}
}

func assertNotContains(t *testing.T, s, substr, msg string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("%s: should not contain %q", msg, substr)
	}
}
