package curator

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// curationResponse is the JSON shape the curation LLM must return.
type curationResponse struct {
	LessonsAppend string `json:"lessons_append"`
	Compact       []struct {
		File    string `json:"file"`
		Content string `json:"content"`
	} `json:"compact"`
}

// parseCuration extracts a curationResponse from LLM output: direct JSON
// parse first, then brace extraction (same pattern as subagent's parsers).
// File names are validated here — anything that is not a bare allowlisted
// filename is dropped from the result.
func parseCuration(output string) (*curationResponse, error) {
	output = strings.TrimSpace(output)

	var resp curationResponse
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		start := strings.Index(output, "{")
		end := strings.LastIndex(output, "}")
		if start < 0 || end <= start {
			return nil, fmt.Errorf("no valid curation JSON found in output")
		}
		if err := json.Unmarshal([]byte(output[start:end+1]), &resp); err != nil {
			return nil, fmt.Errorf("no valid curation JSON found in output")
		}
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
