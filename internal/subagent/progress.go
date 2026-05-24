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

// ProgressResult is the parsed content of _progress.json.
type ProgressResult struct {
	CompletedSteps []string      `json:"completed_steps"`
	BlockedSteps   []BlockedStep `json:"blocked_steps,omitempty"`
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
