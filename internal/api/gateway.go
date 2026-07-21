package api

import "strings"

// Gateway is the single entry point for all incoming messages, regardless of
// the channel they arrive on (HTTP, stream, Telegram, WeChat).
//
// It runs command dispatch in order:
//  1. /cancel — resets the caller's conversation via resetConversation().
//  2. Commander — handles all /commands and config changes.
//  3. Slash-block — unknown /commands are rejected with a help hint instead
//     of falling through to the agent.
//
// If Handle returns handled=false the caller should proceed with its normal
// conversation or agent logic.
//
// asyncSend is an optional callback for async messages (e.g. /auth URL relay).
// resetConversation is called when the user sends /cancel; pass nil if not applicable.
//
// MessageGateway implements botcommon.Gateway (the single canonical gateway
// interface, consumed by every bot) by wrapping a Commander.
type MessageGateway struct {
	commander *Commander
}

// NewMessageGateway creates a Gateway from the given Commander.
// If commander is nil the gateway still handles /cancel and blocks slash commands.
func NewMessageGateway(c *Commander) *MessageGateway {
	return &MessageGateway{commander: c}
}

// Handle processes text through the command gateway.
// Returns (reply, sessionID, true) when the message was a command.
// Returns ("", "", false) when the message should continue to conversation/agent.
func (g *MessageGateway) Handle(text string, asyncSend func(string), resetConversation func()) (reply, sessionID string, handled bool) {
	lower := strings.ToLower(strings.TrimSpace(text))

	if lower == "/cancel" {
		if resetConversation != nil {
			resetConversation()
		}
		return "Conversation cancelled. Send a new message to start over.", "", true
	}

	if g.commander != nil {
		if r, sid, ok := g.commander.Handle(text, asyncSend); ok {
			return r, sid, true
		}
	}
	if strings.HasPrefix(lower, "/") {
		return "Unknown command. Type /help for available commands.", "", true
	}
	return "", "", false
}
