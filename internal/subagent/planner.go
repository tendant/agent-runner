package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/llm"
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

// Planner is a sub-agent that produces a structured plan via a direct LLM API call
// (fast, no CLI overhead). Falls back to executor-based planning when no API
// credentials are configured (via llm.ExecutorClient).
type Planner struct {
	client   llm.Client
	preamble string // prompt template content for context
}

// NewPlanner creates a new planner sub-agent backed by a direct LLM client.
// The preamble is the resolved prompt template content, giving the planner
// visibility into the user's workflow instructions.
func NewPlanner(client llm.Client, preamble string) *Planner {
	return &Planner{client: client, preamble: preamble}
}

// Plan calls the LLM directly and returns a structured plan.
func (p *Planner) Plan(ctx context.Context, workspacePath, message string) (*PlanResult, error) {
	state := ReadWorkspaceState(ctx, workspacePath)

	prompt := p.BuildPrompt(state, message)

	output, err := p.client.Complete(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("planner LLM call failed: %w", err)
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

	if len(state.Skills) > 0 {
		sb.WriteString("Available skills (reference by name in plan steps where relevant):\n")
		for _, s := range state.Skills {
			if s.Description != "" {
				fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
			} else {
				fmt.Fprintf(&sb, "- %s\n", s.Name)
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("User request: ")
	sb.WriteString(message)
	sb.WriteString("\n")

	return sb.String()
}

const revisePrompt = `You are a planning agent revising the plan of a task that is already under way. The user has given feedback. Edit the current plan to address it.

You MUST respond with ONLY a JSON object (no markdown, no explanation) in the same format as the current plan:
{"summary": "...", "approach": "...", "steps": [{"id": "1", "description": "...", "files": [], "done": true}]}

Rules:
- Keep finished steps (done: true) and their IDs unless the feedback means that work must be redone; then set done: false on them.
- Keep the IDs of steps you keep. Give new steps new IDs that don't reuse existing ones.
- Remove steps the feedback makes unnecessary. Add only the steps the feedback requires.
- Keep the summary unless the goal itself changed.
`

// Revise asks the planner to edit plan in response to feedback on a running
// task (TASKS_DESIGN.md §8, plan revision).
func (p *Planner) Revise(ctx context.Context, workspacePath string, plan *PlanResult, feedback string) (*PlanResult, error) {
	prompt := p.BuildRevisePrompt(ReadWorkspaceState(ctx, workspacePath), plan, feedback)
	output, err := p.client.Complete(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("planner LLM call failed: %w", err)
	}
	revised, err := parsePlanResult(output)
	if err != nil {
		return nil, fmt.Errorf("failed to parse planner response: %w (raw: %s)", err, output)
	}
	revised.RawOutput = output
	return revised, nil
}

// BuildRevisePrompt builds the revise-mode planner prompt.
func (p *Planner) BuildRevisePrompt(state WorkspaceState, plan *PlanResult, feedback string) string {
	var sb strings.Builder
	if p.preamble != "" {
		sb.WriteString("## Context from prompt template\n\n")
		sb.WriteString(p.preamble)
		sb.WriteString("\n\n")
	}
	sb.WriteString(revisePrompt)
	sb.WriteString("\n")
	if len(state.RecentCommits) > 0 {
		sb.WriteString("Recent commits:\n")
		sb.WriteString(strings.Join(state.RecentCommits, "\n"))
		sb.WriteString("\n\n")
	}
	current, _ := json.MarshalIndent(plan, "", "  ")
	sb.WriteString("Current plan:\n")
	sb.Write(current)
	sb.WriteString("\n\nUser feedback: ")
	sb.WriteString(feedback)
	sb.WriteString("\n")
	return sb.String()
}

// RevisionIsLarge reports whether a revision should be approved by the user
// before work continues: it reopens finished steps or adds several new ones.
func RevisionIsLarge(before, after *PlanResult) bool {
	was := make(map[string]bool, len(before.Steps))
	for _, s := range before.Steps {
		was[s.ID] = s.Done
	}
	added := 0
	for _, s := range after.Steps {
		done, existed := was[s.ID]
		if !existed {
			added++
		} else if done && !s.Done {
			return true
		}
	}
	return added >= largeRevisionNewSteps
}

// largeRevisionNewSteps is how many added steps make a revision "large".
const largeRevisionNewSteps = 3

// Checklist renders the plan's steps with their status, one per line.
func (p *PlanResult) Checklist() string {
	var sb strings.Builder
	for _, s := range p.Steps {
		mark := "⬜"
		if s.Done {
			mark = "✅"
		}
		fmt.Fprintf(&sb, "%s %s. %s\n", mark, s.ID, s.Description)
	}
	return strings.TrimRight(sb.String(), "\n")
}
