package curator

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/agent-runner/agent-runner/internal/textutil"
)

// curationResponse is the JSON shape the curation LLM must return.
type curationResponse struct {
	LessonsAppend string `json:"lessons_append"`
	Compact       []struct {
		File    string `json:"file"`
		Content string `json:"content"`
	} `json:"compact"`
}

// parseCuration extracts a curationResponse from LLM output. File names are
// validated here — anything that is not a bare allowlisted filename is
// dropped from the result.
func parseCuration(output string) (*curationResponse, error) {
	jsonStr, ok := textutil.ExtractJSON(output)
	if !ok {
		return nil, fmt.Errorf("no valid curation JSON found in output")
	}
	var resp curationResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, fmt.Errorf("no valid curation JSON found in output")
	}

	// Drop any compaction entry whose filename is not a bare allowlisted name.
	valid := resp.Compact[:0]
	for _, item := range resp.Compact {
		if !isAllowedFile(item.File) {
			continue
		}
		valid = append(valid, item)
	}
	resp.Compact = valid
	return &resp, nil
}

// isAllowedFile reports whether name is exactly one of the compactable
// filenames — no paths, no traversal.
func isAllowedFile(name string) bool {
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	return slices.Contains(compactable, name)
}
