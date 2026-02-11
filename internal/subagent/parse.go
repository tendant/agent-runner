package subagent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parsePlanResult extracts a PlanResult from Claude output.
// Tries direct JSON parse first, then falls back to extracting a JSON block.
func parsePlanResult(output string) (*PlanResult, error) {
	output = strings.TrimSpace(output)

	// Try direct parse
	var result PlanResult
	if err := json.Unmarshal([]byte(output), &result); err == nil {
		if len(result.Steps) > 0 {
			return &result, nil
		}
	}

	// Look for JSON object in the output
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		jsonStr := output[start : end+1]
		if err := json.Unmarshal([]byte(jsonStr), &result); err == nil {
			if len(result.Steps) > 0 {
				return &result, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid plan JSON found in output")
}

// parseReviewResult extracts a ReviewResult from Claude output.
// Tries direct JSON parse first, then falls back to extracting a JSON block.
func parseReviewResult(output string) (*ReviewResult, error) {
	output = strings.TrimSpace(output)

	// Try direct parse
	var result ReviewResult
	if err := json.Unmarshal([]byte(output), &result); err == nil {
		return &result, nil
	}

	// Look for JSON object in the output
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		jsonStr := output[start : end+1]
		if err := json.Unmarshal([]byte(jsonStr), &result); err == nil {
			return &result, nil
		}
	}

	return nil, fmt.Errorf("no valid review JSON found in output")
}
