package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/executor"
)

// AnalysisResult is the structured response from the analyzer.
type AnalysisResult struct {
	Action  string `json:"action"`  // "ask", "plan", or "execute"
	Message string `json:"message"` // question text or plan text
	Project string `json:"project"` // resolved project name (may be empty)
}

// Analyzer uses Claude CLI to analyze conversation messages and decide the next action.
type Analyzer struct {
	executor *executor.Executor
}

// NewAnalyzer creates a new conversation analyzer.
func NewAnalyzer(exec *executor.Executor) *Analyzer {
	return &Analyzer{executor: exec}
}

// Analyze sends the conversation history to Claude and gets a structured response.
func (a *Analyzer) Analyze(ctx context.Context, conv *Conversation, availableProjects []string) (*AnalysisResult, error) {
	prompt := a.buildPrompt(conv, availableProjects)

	// Use a temporary directory for execution (no workspace needed for analysis)
	result, err := a.executor.Execute(ctx, "/tmp", prompt)
	if err != nil {
		return nil, fmt.Errorf("analyzer execution failed: %w", err)
	}

	output := result.Output
	if output == "" {
		output = result.RawOutput
	}

	// Parse JSON from output — look for JSON block in case there's surrounding text
	analysisResult, err := parseAnalysisResult(output)
	if err != nil {
		return nil, fmt.Errorf("failed to parse analyzer response: %w (raw: %s)", err, output)
	}

	return analysisResult, nil
}

func (a *Analyzer) buildPrompt(conv *Conversation, availableProjects []string) string {
	var sb strings.Builder

	sb.WriteString(`You are a conversation analyzer for an agent system. Your job is to analyze the user's messages and decide the next action.

You MUST respond with ONLY a JSON object (no markdown, no explanation) in this exact format:
{"action": "ask|plan", "message": "your response text", "project": "project-name-or-empty"}

Rules:
- action "ask": You need more information. "message" should be a clarifying question.
- action "plan": You have enough information to propose a plan. "message" should be a clear plan description that includes the project name.
- NEVER use action "execute" — only "ask" or "plan".
- If the user hasn't specified which project to work on and it's not obvious, ASK.
- If the task is ambiguous or missing key details, ASK.
- When you have enough info, propose a PLAN with a clear description of what will be done.
- Keep questions and plans concise but informative.
- If the user mentions a project name that matches an available project, use it.
- For new projects (not in the list), suggest a project name in the "project" field.

`)

	sb.WriteString("Available projects: ")
	if len(availableProjects) == 0 {
		sb.WriteString("(none yet)")
	} else {
		sb.WriteString(strings.Join(availableProjects, ", "))
	}
	sb.WriteString("\n\n")

	currentProject := conv.GetProject()
	if currentProject != "" {
		fmt.Fprintf(&sb, "Currently selected project: %s\n\n", currentProject)
	}

	sb.WriteString("Conversation history:\n")
	for _, msg := range conv.GetMessages() {
		fmt.Fprintf(&sb, "%s: %s\n", msg.Role, msg.Content)
	}

	return sb.String()
}

// parseAnalysisResult extracts JSON from the analyzer output.
func parseAnalysisResult(output string) (*AnalysisResult, error) {
	output = strings.TrimSpace(output)

	// Try direct parse first
	var result AnalysisResult
	if err := json.Unmarshal([]byte(output), &result); err == nil {
		if result.Action != "" {
			return &result, nil
		}
	}

	// Look for JSON object in the output
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		jsonStr := output[start : end+1]
		if err := json.Unmarshal([]byte(jsonStr), &result); err == nil {
			if result.Action != "" {
				return &result, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid JSON found in output")
}
