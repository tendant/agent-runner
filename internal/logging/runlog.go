package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunLogData contains all data needed to write a run log
type RunLogData struct {
	JobID           string
	Project         string
	Status          string
	Duration        int // seconds
	Instruction     string
	ChangedFiles    []FileChange
	DiffSummary     DiffSummary
	Commit          string
	Branch          string
	ValidationOK    bool
	ValidationError *ValidationResult
	ExecutionLog    string
	Error           string
	ErrorCode       string
}

// FileChange represents a changed file with its stats
type FileChange struct {
	Path       string
	Insertions int
	Deletions  int
}

// DiffSummary contains diff statistics
type DiffSummary struct {
	Insertions int
	Deletions  int
}

// ValidationResult contains validation results
type ValidationResult struct {
	Code    string
	Message string
	Files   []string
}

// AgentLogData contains data for writing an agent session log
type AgentLogData struct {
	SessionID   string
	Project     string
	Status      string
	Duration    int
	PromptFile  string
	Author      string
	Iterations  []AgentIterationLog
	TotalCommits int
	Error       string
}

// AgentIterationLog captures one iteration for the log
type AgentIterationLog struct {
	Iteration    int
	Status       string
	Commit       string
	ChangedFiles []string
	Error        string
	DurationSecs int
}

// RunLogger handles markdown run log generation
type RunLogger struct {
	RunsRoot string
}

// NewRunLogger creates a new run logger
func NewRunLogger(runsRoot string) *RunLogger {
	return &RunLogger{
		RunsRoot: runsRoot,
	}
}

// WriteRunLog writes a markdown log file for a job execution
func (l *RunLogger) WriteRunLog(data *RunLogData) (string, error) {
	// Ensure runs directory exists
	if err := os.MkdirAll(l.RunsRoot, 0755); err != nil {
		return "", fmt.Errorf("failed to create runs directory: %w", err)
	}

	// Generate filename with timestamp
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	filename := fmt.Sprintf("%s_%s.md", timestamp, data.Project)
	filepath := filepath.Join(l.RunsRoot, filename)

	content := l.generateMarkdown(data, timestamp)

	if err := os.WriteFile(filepath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write run log: %w", err)
	}

	return filepath, nil
}

// WriteAgentLog writes a markdown log file for an agent session
func (l *RunLogger) WriteAgentLog(data *AgentLogData) (string, error) {
	if err := os.MkdirAll(l.RunsRoot, 0755); err != nil {
		return "", fmt.Errorf("failed to create runs directory: %w", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	filename := fmt.Sprintf("%s_agent_%s.md", timestamp, data.Project)
	fp := filepath.Join(l.RunsRoot, filename)

	content := l.generateAgentMarkdown(data, timestamp)

	if err := os.WriteFile(fp, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write agent log: %w", err)
	}

	return fp, nil
}

func (l *RunLogger) generateAgentMarkdown(data *AgentLogData, timestamp string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# Agent Session - %s\n\n", strings.ReplaceAll(timestamp, "_", " ")))

	sb.WriteString(fmt.Sprintf("**Session ID:** %s  \n", data.SessionID))
	sb.WriteString(fmt.Sprintf("**Project:** %s  \n", data.Project))
	sb.WriteString(fmt.Sprintf("**Status:** %s  \n", data.Status))
	sb.WriteString(fmt.Sprintf("**Prompt File:** %s  \n", data.PromptFile))
	sb.WriteString(fmt.Sprintf("**Author:** %s  \n", data.Author))
	if data.Duration > 0 {
		sb.WriteString(fmt.Sprintf("**Duration:** %ds  \n", data.Duration))
	}
	sb.WriteString(fmt.Sprintf("**Total Commits:** %d\n\n", data.TotalCommits))

	if data.Error != "" {
		sb.WriteString("## Error\n\n")
		sb.WriteString(fmt.Sprintf("```\n%s\n```\n\n", data.Error))
	}

	if len(data.Iterations) > 0 {
		sb.WriteString("## Iterations\n\n")
		for _, iter := range data.Iterations {
			sb.WriteString(fmt.Sprintf("### Iteration %d — %s\n\n", iter.Iteration, iter.Status))
			if iter.Commit != "" {
				sb.WriteString(fmt.Sprintf("- **Commit:** `%s`\n", iter.Commit))
			}
			if len(iter.ChangedFiles) > 0 {
				sb.WriteString("- **Files:**\n")
				for _, f := range iter.ChangedFiles {
					sb.WriteString(fmt.Sprintf("  - `%s`\n", f))
				}
			}
			if iter.Error != "" {
				sb.WriteString(fmt.Sprintf("- **Error:** %s\n", iter.Error))
			}
			sb.WriteString(fmt.Sprintf("- **Duration:** %ds\n\n", iter.DurationSecs))
		}
	}

	return sb.String()
}

func (l *RunLogger) generateMarkdown(data *RunLogData, timestamp string) string {
	var sb strings.Builder

	// Header
	sb.WriteString(fmt.Sprintf("# Claude Run - %s\n\n", strings.ReplaceAll(timestamp, "_", " ")))

	// Metadata
	sb.WriteString(fmt.Sprintf("**Job ID:** %s  \n", data.JobID))
	sb.WriteString(fmt.Sprintf("**Project:** %s  \n", data.Project))
	sb.WriteString(fmt.Sprintf("**Status:** %s  \n", data.Status))
	if data.Duration > 0 {
		sb.WriteString(fmt.Sprintf("**Duration:** %ds\n", data.Duration))
	}
	sb.WriteString("\n")

	// Instruction
	sb.WriteString("## Instruction\n\n")
	sb.WriteString(fmt.Sprintf("> %s\n\n", data.Instruction))

	// Error (if failed)
	if data.Error != "" {
		sb.WriteString("## Error\n\n")
		if data.ErrorCode != "" {
			sb.WriteString(fmt.Sprintf("**Code:** `%s`\n\n", data.ErrorCode))
		}
		sb.WriteString(fmt.Sprintf("```\n%s\n```\n\n", data.Error))
	}

	// Changed Files
	if len(data.ChangedFiles) > 0 {
		sb.WriteString("## Changed Files\n\n")
		for _, file := range data.ChangedFiles {
			if file.Insertions > 0 || file.Deletions > 0 {
				sb.WriteString(fmt.Sprintf("- `%s` (+%d, -%d)\n", file.Path, file.Insertions, file.Deletions))
			} else {
				sb.WriteString(fmt.Sprintf("- `%s`\n", file.Path))
			}
		}
		sb.WriteString("\n")
	}

	// Diff Summary
	if data.DiffSummary.Insertions > 0 || data.DiffSummary.Deletions > 0 {
		sb.WriteString("## Diff Summary\n\n")
		sb.WriteString(fmt.Sprintf("- Insertions: %d\n", data.DiffSummary.Insertions))
		sb.WriteString(fmt.Sprintf("- Deletions: %d\n\n", data.DiffSummary.Deletions))
	}

	// Commit
	if data.Commit != "" {
		sb.WriteString("## Commit\n\n")
		branch := data.Branch
		if branch == "" {
			branch = "main"
		}
		sb.WriteString(fmt.Sprintf("`%s` pushed to `origin/%s`\n\n", data.Commit, branch))
	}

	// Validation
	sb.WriteString("## Validation\n\n")
	if data.ValidationOK {
		sb.WriteString("\u2713 All changes within allowed paths  \n")
		sb.WriteString("\u2713 No CI config modifications  \n")
		sb.WriteString("\u2713 No secrets detected\n\n")
	} else if data.ValidationError != nil {
		sb.WriteString(fmt.Sprintf("\u2717 **%s**: %s\n\n", data.ValidationError.Code, data.ValidationError.Message))
		if len(data.ValidationError.Files) > 0 {
			sb.WriteString("Violating files:\n")
			for _, f := range data.ValidationError.Files {
				sb.WriteString(fmt.Sprintf("- `%s`\n", f))
			}
			sb.WriteString("\n")
		}
	}

	// Execution Log
	if data.ExecutionLog != "" {
		sb.WriteString("## Execution Log\n\n")
		sb.WriteString("<details>\n")
		sb.WriteString("<summary>Claude Code output</summary>\n\n")
		sb.WriteString("```\n")
		sb.WriteString(data.ExecutionLog)
		sb.WriteString("\n```\n\n")
		sb.WriteString("</details>\n")
	}

	return sb.String()
}
