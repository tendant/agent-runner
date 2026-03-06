package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type progressData struct {
	CompletedSteps []string `json:"completed_steps"`
}

// ReadProgress reads _progress.json from workspacePath and returns the list of
// completed step IDs. Returns an empty slice if the file is missing or invalid.
func ReadProgress(workspacePath string) []string {
	data, err := os.ReadFile(filepath.Join(workspacePath, "_progress.json"))
	if err != nil {
		return nil
	}
	var pd progressData
	if err := json.Unmarshal(data, &pd); err != nil {
		return nil
	}
	return pd.CompletedSteps
}
