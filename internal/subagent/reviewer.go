package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/executor"
)

const reviewerPrompt = `You are a code review agent. Review the workspace to assess whether the task has been completed successfully.

You MUST respond with ONLY a JSON object (no markdown, no explanation) in this exact format:
{
  "complete": true,
  "score": 8,
  "issues": ["description of any remaining issues"],
  "suggestions": ["suggestions for improvement"]
}

Rules:
- "complete": true if the task appears to be done, false if significant work remains
- "score": 1-10 quality rating (10 = perfect, 1 = barely started)
- "issues": list specific problems found (empty array if none)
- "suggestions": list actionable improvements (empty array if none)
`

// Reviewer is a sub-agent that reviews work after the iteration loop (phase 2).
type Reviewer struct {
	executor *executor.Executor
}

// NewReviewer creates a new reviewer sub-agent.
func NewReviewer(exec *executor.Executor) *Reviewer {
	return &Reviewer{executor: exec}
}

// Review runs the reviewer against the workspace and returns a structured review.
func (r *Reviewer) Review(ctx context.Context, reposPath, message string, plan *PlanResult) (*ReviewResult, error) {
	state := ReadWorkspaceState(ctx, reposPath)
	completedIDs := ReadProgress(reposPath)

	prompt := r.buildPrompt(state, message, plan, completedIDs)

	result, err := r.executor.Execute(ctx, reposPath, prompt)
	if err != nil {
		return nil, fmt.Errorf("reviewer execution failed: %w", err)
	}

	output := result.Output
	if output == "" {
		output = result.RawOutput
	}

	review, err := parseReviewResult(output)
	if err != nil {
		return nil, fmt.Errorf("failed to parse reviewer response: %w (raw: %s)", err, output)
	}

	review.RawOutput = output
	return review, nil
}

func (r *Reviewer) buildPrompt(state WorkspaceState, message string, plan *PlanResult, completedIDs []string) string {
	completedSet := make(map[string]bool, len(completedIDs))
	for _, id := range completedIDs {
		completedSet[id] = true
	}

	var sb strings.Builder

	sb.WriteString(reviewerPrompt)
	sb.WriteString("\n")

	sb.WriteString("Original task: ")
	sb.WriteString(message)
	sb.WriteString("\n\n")

	if plan != nil && len(plan.Steps) > 0 {
		sb.WriteString("Plan that was followed:\n")
		for _, step := range plan.Steps {
			check := " "
			if step.Done || completedSet[step.ID] {
				check = "x"
			}
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", check, step.ID, step.Description))
		}
		sb.WriteString("\n")
	}

	if len(state.RepoNames) > 0 {
		sb.WriteString("Repositories: ")
		sb.WriteString(strings.Join(state.RepoNames, ", "))
		sb.WriteString("\n\n")
	}

	if len(state.RecentCommits) > 0 {
		sb.WriteString("Recent commits:\n")
		sb.WriteString(strings.Join(state.RecentCommits, "\n"))
		sb.WriteString("\n\n")
	}

	if state.GitDiffStat != "" {
		sb.WriteString("Uncommitted changes:\n")
		sb.WriteString(state.GitDiffStat)
		sb.WriteString("\n")
	}

	return sb.String()
}
