// Package botcommon holds logic shared by the chat-bot integrations
// (internal/telegram, internal/stream, internal/wechat): the AgentStarter/
// Gateway interfaces each bot depends on, session-progress formatting, and
// the poll loop that watches a running agent session and reports its
// progress. Each bot still owns how it actually delivers a message to its
// platform — botcommon is deliberately transport-agnostic.
package botcommon

import (
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// AgentStarter is the interface bots use to start and poll agent sessions.
type AgentStarter interface {
	StartAgent(message, source, convID string) (sessionID string, err error)
	GetAgentSession(sessionID string) (*agent.Session, bool)
}

// Gateway routes incoming messages through command dispatch before any
// conversation or agent logic. It is the single entry point for all messages.
type Gateway interface {
	Handle(text string, asyncSend func(string), resetConversation func()) (reply, sessionID string, handled bool)
}

// IsConfirmation reports whether text is a positive confirmation.
func IsConfirmation(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "yes", "y", "ok", "sure", "proceed", "go", "do it", "confirm", "yep", "yeah":
		return true
	}
	return false
}

// IsDenial reports whether text is a negative response.
func IsDenial(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "no", "n", "nope", "cancel", "stop", "nah", "nevermind", "never mind":
		return true
	}
	return false
}

// FormatStatusLine returns the "Session completed/failed" line — the part of
// the final-result message that's identical across all three chat bots.
// Callers build the rest of the message around this (output-file counts,
// a preview of the last iteration's output, etc.) and append
// FormatWarningsSuffix last, since each bot places its own extras between
// the status line and the warnings line.
func FormatStatusLine(session *agent.Session) string {
	var sb strings.Builder
	switch session.Status {
	case agent.SessionStatusCompleted:
		fmt.Fprintf(&sb, "Session completed — %d iterations in %ds", len(session.Iterations), session.ElapsedSeconds)
	case agent.SessionStatusStopped:
		fmt.Fprintf(&sb, "Session stopped — %d iterations in %ds", len(session.Iterations), session.ElapsedSeconds)
		if session.Error != "" {
			fmt.Fprintf(&sb, " (%s)", session.Error)
		}
	default:
		fmt.Fprintf(&sb, "Session failed")
		if session.Error != "" {
			fmt.Fprintf(&sb, " — %s", session.Error)
		}
	}
	return sb.String()
}

// FormatWarningsSuffix returns "\n\n⚠ <warnings>" joined by "; ", or "" if
// the session has no warnings. Identical across all three chat bots.
func FormatWarningsSuffix(session *agent.Session) string {
	if len(session.Warnings) == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n⚠ %s", strings.Join(session.Warnings, "; "))
}

// maxIterationOutputChars caps the per-iteration output preview, matching
// the limit telegram/wechat have always used.
const maxIterationOutputChars = 3000

// FormatIterationCore formats a single iteration result. markdownCommit
// wraps the commit hash in backticks (telegram, which renders Markdown);
// wechat has no Markdown rendering and passes false.
func FormatIterationCore(iter agent.IterationResult, markdownCommit bool) string {
	var sb strings.Builder
	switch iter.Status {
	case agent.IterationStatusSuccess:
		fmt.Fprintf(&sb, "Iteration %d: completed", iter.Iteration)
		if iter.Commit != "" {
			if markdownCommit {
				fmt.Fprintf(&sb, " (commit `%s`)", iter.Commit)
			} else {
				fmt.Fprintf(&sb, " (commit %s)", iter.Commit)
			}
		}
	case agent.IterationStatusNoChanges:
		fmt.Fprintf(&sb, "Iteration %d: no changes", iter.Iteration)
	case agent.IterationStatusValidation:
		fmt.Fprintf(&sb, "Iteration %d: validation failed — %s", iter.Iteration, iter.Error)
	case agent.IterationStatusError:
		fmt.Fprintf(&sb, "Iteration %d: error — %s", iter.Iteration, iter.Error)
	default:
		fmt.Fprintf(&sb, "Iteration %d: %s", iter.Iteration, iter.Status)
	}
	if iter.Output != "" {
		output := iter.Output
		if len(output) > maxIterationOutputChars {
			output = output[:maxIterationOutputChars] + "\n... (truncated)"
		}
		fmt.Fprintf(&sb, "\n\n%s", output)
	}
	return sb.String()
}
