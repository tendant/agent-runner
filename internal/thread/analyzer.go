package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
// SetClient swaps the underlying LLM client. Called when configuration
// changes at runtime (/set) so every holder of this Analyzer — the bots and
// the HTTP handlers share one instance — picks up the new model/credentials.
func (a *Analyzer) SetClient(c llm.Client) {
	a.client = c
}

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
// On non-timeout errors (LLM unavailable, parse failure, no credentials) it
// degrades gracefully by returning "execute" so the message still reaches the agent.
func (a *Analyzer) Analyze(ctx context.Context, conv *Thread) (*AnalysisResult, error) {
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
		if ctx.Err() != nil {
			// Real timeout — surface it so the bot can show a meaningful message.
			return nil, fmt.Errorf("analyzer timed out: %w", err)
		}
		// LLM unavailable (no credentials, CLI missing, network error) — degrade
		// gracefully so messages still reach the agent instead of blocking the user.
		slog.Warn("analyzer: LLM call failed, defaulting to execute", "error", err)
		return &AnalysisResult{Action: "execute", Message: "Processing your request..."}, nil
	}

	analysisResult, parseErr := parseAnalysisResult(output)
	if parseErr != nil {
		// Got output but it wasn't valid JSON — LLM returned prose instead of routing JSON.
		// Degrade gracefully rather than blocking the user.
		slog.Warn("analyzer: could not parse LLM response, defaulting to execute", "error", parseErr, "raw", output)
		return &AnalysisResult{Action: "execute", Message: "Processing your request..."}, nil
	}

	return analysisResult, nil
}

func (a *Analyzer) buildPrompt(conv *Thread) string {
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
	sb.WriteString("- action \"execute\": The user's request contains a clear action (verb + target) that requires the agent to DO something (modify files, run code, fetch data, etc.). \"message\" should be a brief confirmation (e.g. \"Adding client Acme Corp\"). This is the DEFAULT for task requests.\n")
	sb.WriteString("- action \"plan\": The request is unusually complex or destructive and warrants user confirmation before proceeding. \"message\" should describe what will be done.\n")

	if a.agentContext != "" {
		sb.WriteString("- action \"ask\": Use for conversational responses that do NOT require the agent to act. This includes: greetings, thanks, capability questions, image/photo analysis or description requests, and questions you can answer directly. Answer directly in \"message\" — do NOT run the agent for these. Use \"execute\" only if the task requires live data or side effects that only the agent can produce.\n")
	} else {
		sb.WriteString("- action \"ask\": Use for conversational responses that do NOT require the agent to act — greetings, thanks, image analysis/description, or anything you can answer directly in \"message\".\n")
	}

	sb.WriteString("- Images/photos sent without a clear task (or with requests like \"what is this?\", \"describe this\", \"look at this\") should use \"ask\" — analyze the image and respond directly.\n")
	sb.WriteString("- NEVER use \"ask\" because you're unsure of task details — the agent knows all context, files, and domain knowledge.\n")
	sb.WriteString("- NEVER ask what \"merge\", \"add\", \"search\", \"delete\", \"update\", or similar action words mean — pass them through to the agent as-is.\n")
	sb.WriteString("- Reminders, timers, and scheduling requests (e.g. \"remind me in 5 minutes\", \"check on X later\") ARE valid actions — use \"execute\" for these.\n")
	sb.WriteString("- When in doubt between \"ask\" and \"execute\": if the response requires the agent to change something or access live state, use \"execute\"; if you can answer from the conversation or the image, use \"ask\".\n\n")

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

// Task message kinds returned by ClassifyTaskMessage.
const (
	TaskMessageContinue = "continue" // an answer, feedback or go-ahead for the current task
	TaskMessageNew      = "new"      // an unrelated new request
	TaskMessageChat     = "chat"     // small talk or an acknowledgement ("thanks!"): no work
)

// ClassifyTaskMessage decides whether a message sent while a task is open
// (waiting for an answer, paused, or recently finished) continues that task
// or is a new, unrelated request. question is the agent's open question, if
// any. Errors and unparseable output return fallback.
func (a *Analyzer) ClassifyTaskMessage(ctx context.Context, goal, question, message, fallback string) string {
	if a == nil || a.client == nil {
		return fallback
	}
	var sb strings.Builder
	sb.WriteString("A user is working with a coding agent on a task. Decide whether their new message continues that task (an answer to the agent's question, feedback or a correction on the work, approval, or a follow-up change to the same work) or is a NEW, unrelated request that should become a separate task.\n\n")
	sb.WriteString("Use \"chat\" for small talk or a bare acknowledgement that asks for no work (\"thanks!\", \"great\", \"ok cool\") — unless it answers the agent's question.\n")
	sb.WriteString("You MUST respond with ONLY a JSON object: {\"kind\": \"continue\"}, {\"kind\": \"new\"} or {\"kind\": \"chat\"}. When unsure, answer \"continue\".\n\n")
	fmt.Fprintf(&sb, "Task goal: %s\n", goal)
	if question != "" {
		fmt.Fprintf(&sb, "The agent asked: %s\n", question)
	}
	fmt.Fprintf(&sb, "New message: %s\n", message)

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	out, err := a.client.Complete(ctx, sb.String())
	if err != nil {
		slog.Warn("analyzer: task message classification failed", "error", err)
		return fallback
	}
	out = strings.TrimSpace(out)
	if i, j := strings.Index(out, "{"), strings.LastIndex(out, "}"); i >= 0 && j > i {
		var r struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(out[i:j+1]), &r) == nil {
			switch strings.ToLower(r.Kind) {
			case TaskMessageNew:
				return TaskMessageNew
			case TaskMessageContinue:
				return TaskMessageContinue
			case TaskMessageChat:
				return TaskMessageChat
			}
		}
	}
	return fallback
}
