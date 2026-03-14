package subagent

import (
	"strings"
	"testing"
)

func TestReviewerBuildPrompt_WithProgressFile(t *testing.T) {
	r := &Reviewer{}
	state := WorkspaceState{
		RecentCommits: []string{"abc123 Initial commit"},
	}
	plan := &PlanResult{
		Summary: "Deploy app",
		Steps: []PlanStep{
			{ID: "1", Description: "Clone repo", Done: false},
			{ID: "2", Description: "Build image", Done: false},
			{ID: "3", Description: "Push image", Done: true},
		},
	}

	// Simulate progress file marking step 1 as completed
	completedIDs := []string{"1"}
	prompt := r.buildPrompt(state, "deploy the app", plan, completedIDs)

	if !strings.Contains(prompt, "[x] 1: Clone repo") {
		t.Error("expected step 1 checked via progress IDs")
	}
	if !strings.Contains(prompt, "[ ] 2: Build image") {
		t.Error("expected step 2 unchecked")
	}
	if !strings.Contains(prompt, "[x] 3: Push image") {
		t.Error("expected step 3 checked via Done flag")
	}
}

func TestReviewerBuildPrompt_NoPlan(t *testing.T) {
	r := &Reviewer{}
	state := WorkspaceState{}

	prompt := r.buildPrompt(state, "do stuff", nil, nil)

	if !strings.Contains(prompt, "Original task: do stuff") {
		t.Error("expected task in prompt")
	}
	if strings.Contains(prompt, "Plan that was followed") {
		t.Error("did not expect plan section with nil plan")
	}
}

func TestReviewerBuildPrompt_NoProgress(t *testing.T) {
	r := &Reviewer{}
	state := WorkspaceState{}
	plan := &PlanResult{
		Steps: []PlanStep{
			{ID: "1", Description: "Step one", Done: false},
			{ID: "2", Description: "Step two", Done: true},
		},
	}

	prompt := r.buildPrompt(state, "task", plan, nil)

	if !strings.Contains(prompt, "[ ] 1: Step one") {
		t.Error("expected step 1 unchecked")
	}
	if !strings.Contains(prompt, "[x] 2: Step two") {
		t.Error("expected step 2 checked via Done flag")
	}
}
