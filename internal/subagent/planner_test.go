package subagent

import (
	"strings"
	"testing"
)

func TestPlanner_BuildPrompt_IncludesSkills(t *testing.T) {
	p := NewPlanner(nil, "")
	state := WorkspaceState{
		Skills: []SkillSummary{
			{Name: "deploy-check", Description: "verifies a deploy went out cleanly"},
			{Name: "no-description-skill"},
		},
	}

	prompt := p.BuildPrompt(state, "do the thing")

	if !strings.Contains(prompt, "Available skills") {
		t.Fatal("expected an 'Available skills' section when skills are present")
	}
	if !strings.Contains(prompt, "- deploy-check: verifies a deploy went out cleanly") {
		t.Errorf("expected skill with description rendered, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- no-description-skill\n") {
		t.Errorf("expected skill without description rendered without a trailing colon, got:\n%s", prompt)
	}
}

func TestPlanner_BuildPrompt_OmitsSkillsSectionWhenNone(t *testing.T) {
	p := NewPlanner(nil, "")
	prompt := p.BuildPrompt(WorkspaceState{}, "do the thing")

	if strings.Contains(prompt, "Available skills") {
		t.Error("expected no 'Available skills' section when there are no skills")
	}
}

func TestRevisionIsLarge(t *testing.T) {
	before := &PlanResult{Steps: []PlanStep{{ID: "1", Done: true}, {ID: "2"}}}
	cases := []struct {
		name  string
		after []PlanStep
		want  bool
	}{
		{"unchanged", []PlanStep{{ID: "1", Done: true}, {ID: "2"}}, false},
		{"two new steps", []PlanStep{{ID: "1", Done: true}, {ID: "2"}, {ID: "3"}, {ID: "4"}}, false},
		{"three new steps", []PlanStep{{ID: "1", Done: true}, {ID: "3"}, {ID: "4"}, {ID: "5"}}, true},
		{"reopens a done step", []PlanStep{{ID: "1"}, {ID: "2"}}, true},
		{"drops steps", []PlanStep{{ID: "1", Done: true}}, false},
	}
	for _, c := range cases {
		if got := RevisionIsLarge(before, &PlanResult{Steps: c.after}); got != c.want {
			t.Errorf("%s: RevisionIsLarge = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPlanChecklistAndRevisePrompt(t *testing.T) {
	plan := &PlanResult{Summary: "s", Steps: []PlanStep{{ID: "1", Description: "build", Done: true}, {ID: "2", Description: "ship"}}}
	if got := plan.Checklist(); got != "✅ 1. build\n⬜ 2. ship" {
		t.Errorf("Checklist = %q", got)
	}
	prompt := NewPlanner(nil, "PREAMBLE").BuildRevisePrompt(WorkspaceState{}, plan, "ship to staging")
	for _, want := range []string{"PREAMBLE", "revising the plan", `"description": "ship"`, "User feedback: ship to staging"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("revise prompt missing %q", want)
		}
	}
}
