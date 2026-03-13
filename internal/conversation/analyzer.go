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
	executor executor.Executor
}

// NewAnalyzer creates a new conversation analyzer.
func NewAnalyzer(exec executor.Executor) *Analyzer {
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

	sb.WriteString(`You are a conversation router for a specialized agent. The agent already has full domain knowledge via its own prompt — it knows what system it works with, what files to manage, and how to handle requests. Your ONLY job is to decide whether to run the agent immediately, confirm first, or ask for clarification.

You MUST respond with ONLY a JSON object (no markdown, no explanation) in this exact format:
{"action": "ask|plan|execute", "message": "your response text"}

Rules:
- action "execute": The user's request contains a clear action (verb + target). Run immediately. "message" should be a brief description (e.g. "Adding client Acme Corp", "Merging zidong clients", "Searching conflicts for Acme"). This is the DEFAULT — use it whenever the user expresses intent to do something.
- action "plan": The request is unusually complex or destructive and warrants user confirmation before proceeding. "message" should describe what will be done.
- action "ask": The message contains NO actionable request at all (e.g. just "hello" or a question). "message" should be a response or clarifying question.
- NEVER ask about systems, databases, files, context, or implementation details — the agent already knows all of that.
- NEVER ask what "merge", "add", "search", "delete", "update", or similar action words mean — pass them through to the agent as-is.
- Reminders, timers, and scheduling requests (e.g. "remind me in 5 minutes", "check on X later", "schedule Y every Monday") ARE valid actions — use "execute" for these. The agent can schedule tasks.
- When in doubt, use "execute". The agent is better equipped to handle the request than you are.

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
