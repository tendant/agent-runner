package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/executor"
)

const plannerPrompt = `You are a planning agent. Analyze the workspace and the user's request, then produce a structured plan.

You MUST respond with ONLY a JSON object (no markdown, no explanation) in this exact format:
{
  "summary": "Brief one-line summary of the task",
  "approach": "High-level description of the approach you will take",
  "steps": [
    {"id": "1", "description": "First concrete step", "files": ["path/to/file.go"], "done": false},
    {"id": "2", "description": "Second concrete step", "files": [], "done": false}
  ]
}

Rules:
- Produce 3-10 concrete, actionable steps
- Each step should be small enough to complete in one iteration
- Include relevant file paths in the "files" array when known
- All steps start with "done": false
- The summary should capture the goal, not the method
- The approach should describe the strategy at a high level
`

// Planner is a sub-agent that produces a structured plan before the iteration loop.
type Planner struct {
	executor *executor.Executor
}

// NewPlanner creates a new planner sub-agent.
func NewPlanner(exec *executor.Executor) *Planner {
	return &Planner{executor: exec}
}

// Plan runs the planner against the workspace and returns a structured plan.
func (p *Planner) Plan(ctx context.Context, reposPath, message string) (*PlanResult, error) {
	state := ReadWorkspaceState(ctx, reposPath)

	prompt := p.buildPrompt(state, message)

	result, err := p.executor.Execute(ctx, reposPath, prompt)
	if err != nil {
		return nil, fmt.Errorf("planner execution failed: %w", err)
	}

	output := result.Output
	if output == "" {
		output = result.RawOutput
	}

	plan, err := parsePlanResult(output)
	if err != nil {
		return nil, fmt.Errorf("failed to parse planner response: %w (raw: %s)", err, output)
	}

	plan.RawOutput = output
	return plan, nil
}

func (p *Planner) buildPrompt(state WorkspaceState, message string) string {
	var sb strings.Builder

	sb.WriteString(plannerPrompt)
	sb.WriteString("\n")

	if len(state.RepoNames) > 0 {
		sb.WriteString("Repositories in workspace: ")
		sb.WriteString(strings.Join(state.RepoNames, ", "))
		sb.WriteString("\n\n")
	}

	if state.TodoContent != "" {
		sb.WriteString("Current TODO.md:\n")
		sb.WriteString(state.TodoContent)
		sb.WriteString("\n\n")
	}

	if len(state.RecentCommits) > 0 {
		sb.WriteString("Recent commits:\n")
		sb.WriteString(strings.Join(state.RecentCommits, "\n"))
		sb.WriteString("\n\n")
	}

	sb.WriteString("User request: ")
	sb.WriteString(message)
	sb.WriteString("\n")

	return sb.String()
}
