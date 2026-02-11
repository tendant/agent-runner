package subagent

import (
	"context"
	"fmt"
	"strings"
)

// PromptBuilder assembles dynamic iteration prompts by combining
// the preamble (resolved template), plan state, and workspace state.
type PromptBuilder struct {
	preamble string
}

// NewPromptBuilder creates a prompt builder with the given preamble.
// The preamble is the already-resolved prompt template content.
func NewPromptBuilder(preamble string) *PromptBuilder {
	return &PromptBuilder{preamble: preamble}
}

// Build creates a dynamic prompt for the given iteration, injecting plan
// progress, workspace state, and iteration metadata.
func (pb *PromptBuilder) Build(ctx context.Context, reposPath string, plan *PlanResult, iteration int, message string) string {
	state := ReadWorkspaceState(ctx, reposPath)

	var sb strings.Builder

	// Preamble (resolved template)
	sb.WriteString(pb.preamble)
	sb.WriteString("\n\n")

	// Plan with checkboxes
	if plan != nil && len(plan.Steps) > 0 {
		sb.WriteString("## Plan\n\n")
		if plan.Summary != "" {
			sb.WriteString(fmt.Sprintf("**Goal:** %s\n", plan.Summary))
		}
		if plan.Approach != "" {
			sb.WriteString(fmt.Sprintf("**Approach:** %s\n\n", plan.Approach))
		}
		for _, step := range plan.Steps {
			check := " "
			if step.Done {
				check = "x"
			}
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s", check, step.ID, step.Description))
			if len(step.Files) > 0 {
				sb.WriteString(fmt.Sprintf(" (%s)", strings.Join(step.Files, ", ")))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// TODO.md contents
	if state.TodoContent != "" {
		sb.WriteString("## Current TODO.md\n\n")
		sb.WriteString(state.TodoContent)
		sb.WriteString("\n\n")
	}

	// Recent commits
	if len(state.RecentCommits) > 0 {
		sb.WriteString("## Recent Commits\n\n")
		for _, c := range state.RecentCommits {
			sb.WriteString("- " + c + "\n")
		}
		sb.WriteString("\n")
	}

	// Uncommitted changes
	if state.GitDiffStat != "" {
		sb.WriteString("## Uncommitted Changes\n\n")
		sb.WriteString("```\n")
		sb.WriteString(state.GitDiffStat)
		sb.WriteString("\n```\n\n")
	}

	// Iteration metadata
	sb.WriteString(fmt.Sprintf("**Iteration:** %d\n\n", iteration))
	sb.WriteString("Continue working on the task. Pick up where you left off and make progress on the next unchecked step in the plan.\n")

	return sb.String()
}

// BuildStatic returns the preamble unchanged. Used for backward compatibility
// when the planner is disabled.
func (pb *PromptBuilder) BuildStatic(message string) string {
	return pb.preamble
}
