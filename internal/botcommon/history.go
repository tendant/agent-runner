package botcommon

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// HistoryMessage is one message of a thread's history as the chat server
// keeps it: every message in the thread, whoever sent it, not only the ones
// this bot handled.
type HistoryMessage struct {
	Speaker string // who said it, as the agent should read it ("User 6a31", "You (the assistant)", ...)
	Content string
	Time    time.Time
	Self    bool // posted by this bot
}

// HistoryFunc returns a thread's full history, oldest first, the current
// message included. Transports whose server keeps history (agent-stream)
// set Engine.History; the others fall back to the bot's own record.
type HistoryFunc func(ctx context.Context, id string) ([]HistoryMessage, error)

const (
	// maxHistoryChars bounds the history a new task's prompt starts with.
	maxHistoryChars = 12000
	// maxRecentChars bounds the recent messages a continuing turn gets
	// next to its context block (the agent's resumed conversation has the
	// rest).
	maxRecentChars = 4000
	// maxHistoryMessageChars caps one message; attachment references are
	// kept past the cap.
	maxHistoryMessageChars = 2000
)

// historyAttachmentRef matches the references bots put in a message for
// files the user attached ([Image: /path], [File 'name': /path]).
var historyAttachmentRef = regexp.MustCompile(`\[(?:Image: [^\]]+|File '[^']*': [^\]]+)\]`)

// FormatHistory renders msgs for a prompt, newest kept when they don't all
// fit in maxChars; "" for none.
func FormatHistory(msgs []HistoryMessage, maxChars int) string {
	var lines []string
	used, omitted := 0, 0
	for i := len(msgs) - 1; i >= 0; i-- {
		line := msgs[i].Speaker + ": " + capMessage(msgs[i].Content)
		if used+len(line) > maxChars && len(lines) > 0 {
			omitted = i + 1
			break
		}
		lines = append(lines, line)
		used += len(line) + 2
	}
	if len(lines) == 0 {
		return ""
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	out := strings.Join(lines, "\n\n")
	if omitted > 0 {
		out = fmt.Sprintf("[%d earlier messages omitted]\n\n%s", omitted, out)
	}
	return out
}

func capMessage(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxHistoryMessageChars {
		return s
	}
	short := string(r[:maxHistoryMessageChars]) + " …"
	for _, ref := range historyAttachmentRef.FindAllString(s, -1) {
		if !strings.Contains(short, ref) {
			short += " " + ref
		}
	}
	return short
}

// threadHistory is the thread's history before its current message, from
// the chat server when the transport provides it (ok), so nothing the bot
// forgot (an idle thread, a restart, messages it wasn't addressed) is lost.
func (e *Engine) threadHistory(ctx context.Context, id string) (prior []HistoryMessage, ok bool) {
	if e.History == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	msgs, err := e.History(ctx, id)
	if err != nil {
		slog.Warn(e.Label+": could not fetch thread history, using the bot's own", "id", id, "error", err)
		return nil, false
	}
	// Drop the current message (the latest one not from this bot) and
	// anything the bot posted after it.
	for len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		msgs = msgs[:len(msgs)-1]
		if !last.Self {
			break
		}
	}
	return msgs, true
}
