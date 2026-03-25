package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/llm"
)

var imagePathRe = regexp.MustCompile(`\[Image:\s*([^\]]+)\]`)

// AnalysisResult is the structured response from the analyzer.
type AnalysisResult struct {
	Action  string `json:"action"`  // "ask", "plan", or "execute"
	Message string `json:"message"` // question text or plan text
}

// Analyzer uses a fast LLM client to analyze conversation messages and decide
// the next action. It falls back gracefully when no client is configured.
type Analyzer struct {
	client       llm.Client
	agentContext string        // optional: agent system prompt snippet for accurate routing
	timeout      time.Duration // per-call timeout; defaults to 30s
}

const defaultAnalyzerTimeout = 30 * time.Second

// NewAnalyzer creates a new conversation analyzer backed by the given LLM client.
func NewAnalyzer(client llm.Client) *Analyzer {
	return &Analyzer{client: client, timeout: defaultAnalyzerTimeout}
}

// SetTimeout overrides the per-call LLM timeout. Useful for slow local models.
func (a *Analyzer) SetTimeout(d time.Duration) {
	a.timeout = d
}

// SetAgentContext provides the analyzer with the agent's system prompt so it
// can make accurate routing decisions and give correct responses for greetings.
func (a *Analyzer) SetAgentContext(context string) {
	a.agentContext = context
}

// Summarize condenses conversation history into a short summary.
func (a *Analyzer) Summarize(ctx context.Context, messages []Message) (string, error) {
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation in 2-3 sentences, preserving key decisions, requests, and outcomes. Output ONLY the summary text, no preamble.\n\n")
	for _, msg := range messages {
		fmt.Fprintf(&sb, "%s: %s\n", msg.Role, msg.Content)
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	result, err := a.client.Complete(ctx, sb.String())
	if err != nil {
		return "", fmt.Errorf("summarization failed: %w", err)
	}
	return strings.TrimSpace(result), nil
}

// Analyze sends the conversation history to the LLM and returns a routing decision.
func (a *Analyzer) Analyze(ctx context.Context, conv *Conversation) (*AnalysisResult, error) {
	prompt := a.buildPrompt(conv)

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// Extract image paths from conversation messages for vision-capable clients.
	var (
		output string
		err    error
	)
	if mc, ok := a.client.(llm.MultimodalClient); ok {
		var imagePaths []string
		for _, msg := range conv.GetMessages() {
			for _, m := range imagePathRe.FindAllStringSubmatch(msg.Content, -1) {
				if len(m) > 1 {
					imagePaths = append(imagePaths, strings.TrimSpace(m[1]))
				}
			}
		}
		output, err = mc.CompleteWithImages(ctx, prompt, imagePaths)
	} else {
		output, err = a.client.Complete(ctx, prompt)
	}
	if err != nil {
		return nil, fmt.Errorf("analyzer failed: %w", err)
	}

	analysisResult, err := parseAnalysisResult(output)
	if err != nil {
		return nil, fmt.Errorf("failed to parse analyzer response: %w (raw: %s)", err, output)
	}

	return analysisResult, nil
}

func (a *Analyzer) buildPrompt(conv *Conversation) string {
	var sb strings.Builder

	if a.agentContext != "" {
		sb.WriteString("The agent you are routing for has the following purpose and capabilities:\n\n")
		sb.WriteString(a.agentContext)
		sb.WriteString("\n\n---\n\n")
	}

	sb.WriteString("You are a conversation router for a specialized agent. The agent already has full domain knowledge via its own prompt — it knows what system it works with, what files to manage, and how to handle requests. Your ONLY job is to decide whether to run the agent immediately, confirm first, or ask for clarification.\n\n")
	sb.WriteString("You MUST respond with ONLY a JSON object (no markdown, no explanation) in this exact format:\n")
	sb.WriteString("{\"action\": \"ask|plan|execute\", \"message\": \"your response text\"}\n\n")
	sb.WriteString("Rules:\n")
	sb.WriteString("- action \"execute\": The user's request contains a clear action (verb + target). Run immediately. \"message\" should be a brief description (e.g. \"Adding client Acme Corp\", \"Searching conflicts for Acme\"). This is the DEFAULT — use it whenever the user expresses intent to do something.\n")
	sb.WriteString("- action \"plan\": The request is unusually complex or destructive and warrants user confirmation before proceeding. \"message\" should describe what will be done.\n")

	if a.agentContext != "" {
		sb.WriteString("- action \"ask\": The user sent something with NO actionable intent (e.g. \"hello\", \"thanks\", \"ok\", or a general capability question). Answer general capability questions (\"what can you do\", \"help\", \"how does this work\") directly from the agent context above — no need to run the agent. Use \"execute\" if the question requires accessing live data or state that only the agent can retrieve.\n")
	} else {
		sb.WriteString("- action \"ask\": The user sent something with NO actionable intent and no way to clarify further (e.g. just \"hello\", \"thanks\", \"ok\"). For capability questions (\"what can you do\", \"help\"), use \"execute\" instead so the agent can answer with accurate context.\n")
	}

	sb.WriteString("- NEVER use \"ask\" because you're unsure of task details — the agent already knows all context, files, systems, and domain knowledge.\n")
	sb.WriteString("- NEVER ask what \"merge\", \"add\", \"search\", \"delete\", \"update\", or similar action words mean — pass them through to the agent as-is.\n")
	sb.WriteString("- Reminders, timers, and scheduling requests (e.g. \"remind me in 5 minutes\", \"check on X later\") ARE valid actions — use \"execute\" for these.\n")
	sb.WriteString("- When in doubt, use \"execute\". The agent is always better equipped to handle the request than you are.\n\n")

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
