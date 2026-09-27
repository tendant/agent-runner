package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// BlockedStep records a plan step that the agent cannot proceed with.
type BlockedStep struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// Turn statuses an agent may report in _progress.json during a multi-turn
// task (TASKS_DESIGN.md §5). Absent means "working".
const (
	TurnWorking    = "working"
	TurnNeedsInput = "needs_input"
	TurnDone       = "done"
)

// ProgressResult is the parsed content of _progress.json.
type ProgressResult struct {
	CompletedSteps []string      `json:"completed_steps"`
	BlockedSteps   []BlockedStep `json:"blocked_steps,omitempty"`

	// Multi-turn task fields; ignored outside task mode.
	Status    string   `json:"status,omitempty"`
	Question  string   `json:"question,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Decisions []string `json:"decisions,omitempty"`
}

// ReadProgress reads _progress.json from workspacePath.
// Returns a zero ProgressResult if the file is missing or invalid.
func ReadProgress(workspacePath string) ProgressResult {
	data, err := os.ReadFile(filepath.Join(workspacePath, "_progress.json"))
	if err != nil {
		return ProgressResult{}
	}
	var result ProgressResult
	if err := json.Unmarshal(data, &result); err != nil {
		return ProgressResult{}
	}
	return result
}

// ResetTurnFields clears the per-turn fields of _progress.json at the start
// of a task turn, keeping completed steps. Without it a question or "done"
// left by the previous turn would end the new turn at its first iteration,
// and a stale blocked step would stop it outright. A missing file is fine.
func ResetTurnFields(workspacePath string) error {
	path := filepath.Join(workspacePath, "_progress.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	p := ReadProgress(workspacePath)
	p.BlockedSteps = nil
	p.Status, p.Question, p.Summary, p.Decisions = "", "", "", nil
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// SyncCompletedSteps rewrites _progress.json's completed_steps to the plan's
// done steps — after a plan revision, so a step the revision reopened isn't
// marked done again from the stale list. Other fields are kept.
func SyncCompletedSteps(workspacePath string, plan *PlanResult) error {
	p := ReadProgress(workspacePath)
	p.CompletedSteps = nil
	for _, s := range plan.Steps {
		if s.Done {
			p.CompletedSteps = append(p.CompletedSteps, s.ID)
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspacePath, "_progress.json"), data, 0o644)
}

// planFile is where a task keeps its plan between turns: the runner's state
// directory, beside workspace/, which the agent does not see.
func planFile(taskDir string) string { return filepath.Join(taskDir, "state", "plan.json") }

// SavePlan stores a task's plan so later turns continue it instead of
// planning from scratch.
func SavePlan(taskDir string, plan *PlanResult) error {
	if plan == nil {
		return nil
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(planFile(taskDir)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(planFile(taskDir), data, 0o644)
}

// LoadPlan returns the plan saved by an earlier turn, or nil.
func LoadPlan(taskDir string) *PlanResult {
	data, err := os.ReadFile(planFile(taskDir))
	if err != nil {
		return nil
	}
	var plan PlanResult
	if err := json.Unmarshal(data, &plan); err != nil || len(plan.Steps) == 0 {
		return nil
	}
	return &plan
}
