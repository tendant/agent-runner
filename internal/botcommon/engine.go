package botcommon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/conversation"
)

// Sender abstracts a chat transport's outbound message primitives. id is the
// conversation key used by conversation.Manager; each bot converts it to its
// native address type (Telegram parses it back to an int64 chat ID, WeChat
// uses it as the user ID, Stream as the conversation ID).
type Sender interface {
	// Status sends a transient progress note ("Working on it...").
	Status(ctx context.Context, id, text string)
	// Reply sends a mid-turn assistant message; streaming transports keep
	// their delta stream open.
	Reply(ctx context.Context, id, text string)
	// Final sends a turn-concluding message; streaming transports close the
	// stream here.
	Final(ctx context.Context, id, text string)
}

// Engine drives the conversation flow shared by all chat bots:
// confirm → start agent → poll/report → post-process → drain queued input.
// Transport-specific behavior is injected via Sender, NewReporter, and
// OnSessionDone; cosmetic differences via the announce options.
type Engine struct {
	Starter     AgentStarter
	ConvManager *conversation.Manager
	Analyzer    *conversation.Analyzer // nil = confirmation-only flow (no intent routing)
	Sender      Sender
	Source      string // session source tag, e.g. "telegram"
	Label       string // log prefix, e.g. "telegram"

	StartText            string // sent when an agent run begins, e.g. "Starting agent..."
	SessionStartedFormat string // fmt string for the session-started note; "" = don't send
	AnnounceQueued       bool   // send "Processing queued messages..." before re-dispatching pending input

	// OnSessionDone runs transport-specific post-processing (output-file
	// upload) after a session ends and its result has been reported.
	OnSessionDone func(ctx context.Context, id string, session *agent.Session)
	// NewReporter builds the per-session progress reporter for PollAndReport.
	NewReporter func(id string) Reporter

	WG *sync.WaitGroup
}

// HandleConfirmation starts an agent session from the conversation's latest
// user message (with history context) and reports progress until it ends.
func (e *Engine) HandleConfirmation(ctx context.Context, id string, conv *conversation.Conversation) {
	e.Sender.Status(ctx, id, e.StartText)
	conv.SetState(conversation.StateExecuting)

	// Build message: latest user message + conversation history for context.
	messages := conv.GetMessages()
	var currentMsg string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			currentMsg = messages[i].Content
			break
		}
	}
	message := currentMsg
	if history := conv.GetFormattedHistory(); history != "" {
		message = fmt.Sprintf("## Conversation History\n\n%s\n\n## Current Request\n\n%s", history, currentMsg)
	}

	sessionID, err := e.Starter.StartAgent(message, e.Source)
	if err != nil {
		conv.SetState(conversation.StateGathering)
		e.Sender.Final(ctx, id, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	slog.Info(e.Label+": agent session started", "id", id, "session_id", sessionID)
	if e.SessionStartedFormat != "" {
		e.Sender.Reply(ctx, id, fmt.Sprintf(e.SessionStartedFormat, sessionID))
	}

	e.WG.Go(func() {
		PollAndReport(e.Starter, sessionID, e.NewReporter(id))

		// The result has been reported. Collect session data and check for
		// pending messages before touching conversation state.
		session, sessionOk := e.Starter.GetAgentSession(sessionID)
		if sessionOk {
			for _, iter := range session.Iterations {
				if iter.Output != "" {
					conv.AddMessage("assistant", iter.Output)
				}
			}
		}
		hasPending := conv.ClearPendingInput()

		// Clear StateExecuting before slow post-processing so new messages
		// are accepted immediately instead of being queued.
		if !hasPending {
			e.ConvManager.Complete(id)
		}

		if sessionOk && len(session.OutputFiles) > 0 && e.OnSessionDone != nil {
			e.OnSessionDone(ctx, id, session)
		}
		if e.Analyzer != nil && conv.NeedsCompaction() {
			SummarizeConversation(e.Analyzer, conv, e.Label)
		}

		// Process messages that arrived during execution.
		if hasPending {
			conv.SetState(conversation.StateGathering)
			if e.AnnounceQueued {
				e.Sender.Reply(ctx, id, "Processing queued messages...")
			}
			if e.Analyzer == nil {
				e.HandleConfirmation(ctx, id, conv)
			} else {
				e.HandleAnalysis(ctx, id, conv)
			}
		}
	})
}

// HandleAnalysis routes the conversation through the intent analyzer and
// dispatches on its decision (execute / ask / plan).
func (e *Engine) HandleAnalysis(ctx context.Context, id string, conv *conversation.Conversation) {
	analysisCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := e.Analyzer.Analyze(analysisCtx, conv)
	if err != nil {
		slog.Error(e.Label+": analyzer error", "error", err)
		if analysisCtx.Err() == context.DeadlineExceeded {
			e.Sender.Final(ctx, id, "Sorry, the request timed out. Please try again.")
		} else {
			e.Sender.Final(ctx, id, "Sorry, I had trouble understanding that. Could you rephrase?")
		}
		return
	}

	switch result.Action {
	case "execute":
		// High-confidence action — skip confirmation, run immediately.
		conv.AddMessage("assistant", result.Message)
		e.Sender.Reply(ctx, id, result.Message)
		e.HandleConfirmation(ctx, id, conv)

	case "ask":
		conv.AddMessage("assistant", result.Message)
		e.Sender.Final(ctx, id, result.Message)

	case "plan":
		conv.SetPlan(result.Message)
		conv.AddMessage("assistant", result.Message)
		e.Sender.Final(ctx, id, result.Message+"\n\nProceed? (yes/no)")

	default:
		// Unknown action — treat as "ask".
		conv.AddMessage("assistant", result.Message)
		e.Sender.Final(ctx, id, result.Message)
	}
}
