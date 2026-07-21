package subagent

import (
	"encoding/json"
	"fmt"

	"github.com/agent-runner/agent-runner/internal/textutil"
)

// parsePlanResult extracts a PlanResult from agent CLI output.
func parsePlanResult(output string) (*PlanResult, error) {
	jsonStr, ok := textutil.ExtractJSON(output)
	if !ok {
		return nil, fmt.Errorf("no valid plan JSON found in output")
	}
	var result PlanResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil || len(result.Steps) == 0 {
		return nil, fmt.Errorf("no valid plan JSON found in output")
	}
	return &result, nil
}

// parseReviewResult extracts a ReviewResult from agent CLI output.
func parseReviewResult(output string) (*ReviewResult, error) {
	jsonStr, ok := textutil.ExtractJSON(output)
	if !ok {
		return nil, fmt.Errorf("no valid review JSON found in output")
	}
	var result ReviewResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, fmt.Errorf("no valid review JSON found in output")
	}
	return &result, nil
}
