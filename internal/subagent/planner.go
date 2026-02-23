package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/executor"
)

const plannerPrompt = `You are a planning agent. Analyze the workspace, the prompt template instructions, and the user's request, then produce a structured plan.

CRITICAL: If a prompt template is provided above, your plan MUST follow its workflow exactly. The template defines the required steps (e.g., creating repos, infrastructure setup, git operations). Do NOT skip or simplify steps from the template. Include ALL steps the template requires, even if the workspace already has some repos.

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
- Produce 3-15 concrete, actionable steps
- Each step should be small enough to complete in one iteration
- Include relevant file paths in the "files" array when known
- All steps start with "done": false
- The summary should capture the goal, not the method
- The approach should describe the strategy at a high level
- Steps must cover the FULL workflow from the prompt template, including infrastructure, git operations, and deployment setup

After completing a plan step, update ` + "`_progress.json`" + ` in the workspace root with: {"completed_steps": ["1", "2"]} listing all completed step IDs.
`

// Planner is a sub-agent that produces a structured plan before the iteration loop.
type Planner struct {
	executor *executor.Executor
	preamble string // prompt template content for context
}

// NewPlanner creates a new planner sub-agent.
// The preamble is the resolved prompt template content, giving the planner
// visibility into the user's workflow instructions.
func NewPlanner(exec *executor.Executor, preamble string) *Planner {
	return &Planner{executor: exec, preamble: preamble}
}

// Plan runs the planner against the workspace and returns a structured plan.
func (p *Planner) Plan(ctx context.Context, reposPath, message string) (*PlanResult, error) {
	state := ReadWorkspaceState(ctx, reposPath)

	prompt := p.BuildPrompt(state, message)

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

// BuildPrompt builds the full planner prompt (exported for logging/debugging).
func (p *Planner) BuildPrompt(state WorkspaceState, message string) string {
	var sb strings.Builder

	// Inject preamble so the planner sees the user's workflow instructions
	if p.preamble != "" {
		sb.WriteString("## Context from prompt template\n\n")
		sb.WriteString(p.preamble)
		sb.WriteString("\n\n")
	}

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
