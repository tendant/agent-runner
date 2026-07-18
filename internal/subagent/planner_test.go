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
