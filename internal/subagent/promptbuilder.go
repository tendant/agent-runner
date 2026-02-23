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
// progress, workspace state, and iteration metadata. errorContext, when
// non-empty, is rendered before the iteration line so Claude can see what
// went wrong on the previous attempt.
func (pb *PromptBuilder) Build(ctx context.Context, reposPath string, plan *PlanResult, iteration int, message string, errorContext string) string {
	state := ReadWorkspaceState(ctx, reposPath)

	var sb strings.Builder

	// Preamble (resolved template)
	sb.WriteString(pb.preamble)
	sb.WriteString("\n\n")

	// Plan with checkboxes
	if plan != nil && len(plan.Steps) > 0 {
		// Read progress file and build set of completed step IDs
		completedIDs := ReadProgress(reposPath)
		completedSet := make(map[string]bool, len(completedIDs))
		for _, id := range completedIDs {
			completedSet[id] = true
		}

		sb.WriteString("## Plan (guide only — follow the workflow instructions above)\n\n")
		if plan.Summary != "" {
			sb.WriteString(fmt.Sprintf("**Goal:** %s\n", plan.Summary))
		}
		if plan.Approach != "" {
			sb.WriteString(fmt.Sprintf("**Approach:** %s\n\n", plan.Approach))
		}
		for _, step := range plan.Steps {
			check := " "
			if step.Done || completedSet[step.ID] {
				check = "x"
			}
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s", check, step.ID, step.Description))
			if len(step.Files) > 0 {
				sb.WriteString(fmt.Sprintf(" (%s)", strings.Join(step.Files, ", ")))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		sb.WriteString("**Important:** The instructions above are the source of truth. This plan is a rough guide — do not skip steps from the workflow instructions even if they are not listed in the plan.\n\n")
		sb.WriteString("After completing a plan step, update `_progress.json` in the workspace root with: `{\"completed_steps\": [\"1\", \"2\"]}` listing all completed step IDs.\n\n")
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

	// Error context from previous iteration (if any)
	if errorContext != "" {
		sb.WriteString(errorContext)
		sb.WriteString("\n")
	}

	// Iteration metadata — no workflow instructions here; the preamble drives behavior
	sb.WriteString(fmt.Sprintf("**Iteration:** %d\n", iteration))

	return sb.String()
}

// BuildStatic returns the preamble, optionally with error context appended.
// Used for backward compatibility when the planner is disabled.
func (pb *PromptBuilder) BuildStatic(message string, errorContext string) string {
	if errorContext != "" {
		return pb.preamble + "\n\n" + errorContext
	}
	return pb.preamble
}
