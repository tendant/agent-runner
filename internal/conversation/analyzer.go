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
func (a *Analyzer) Analyze(ctx context.Context, conv *Conversation) (*AnalysisResult, error) {
	prompt := a.buildPrompt(conv)

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

func (a *Analyzer) buildPrompt(conv *Conversation) string {
	var sb strings.Builder

	sb.WriteString(`You are a conversation analyzer for an agent system. Your job is to analyze the user's messages and decide the next action.

You MUST respond with ONLY a JSON object (no markdown, no explanation) in this exact format:
{"action": "ask|plan|execute", "message": "your response text"}

Rules:
- action "execute": The user's request is a common, well-known command and you are certain what needs to be done. Skip confirmation and run immediately. "message" should be a brief description of what will be done (e.g. "Adding client Acme Corp", "Searching conflicts for Acme").
- action "plan": You have enough information but the task is complex or unusual enough to warrant user confirmation. "message" should describe what will be done.
- action "ask": You need more information because the core request is truly ambiguous. "message" should be a clarifying question.
- BIAS TOWARD ACTION: prefer "execute" for straightforward, common actions (add, search, list, delete, update). Use "plan" only for complex or multi-step tasks. Use "ask" only when the core request is truly ambiguous.
- Keep messages concise but informative.

`)

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
