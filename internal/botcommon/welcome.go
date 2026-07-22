package botcommon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// DefaultWelcomeText is sent on a conversation's first contact when no
// MEMORY_DIR/WELCOME.md override exists.
const DefaultWelcomeText = `👋 I'm your agent runner. Send me a task in plain language — I'll plan it, run it, and report back.

For example:
• "Summarize the open TODOs across the shared repos"
• "Fix the typo on the landing page and push it"

Type /help for commands, /status to see what I'm doing.`

// Welcome configures the one-time first-contact greeting.
type Welcome struct {
	Enabled  bool
	Text     string // composed greeting; use LoadWelcomeText
	StateDir string // directory for per-conversation welcomed markers
}

// LoadWelcomeText returns the contents of memoryDir/WELCOME.md when present
// and non-empty, else DefaultWelcomeText.
func LoadWelcomeText(memoryDir string) string {
	if memoryDir != "" {
		if data, err := os.ReadFile(filepath.Join(memoryDir, "WELCOME.md")); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	return DefaultWelcomeText
}

// WelcomeIfNeeded sends the welcome text the first time a conversation id is
// seen, tracked by a marker file in Welcome.StateDir. Errors are non-fatal:
// an unwritable state dir means the greeting may repeat, never that a
// message is lost. Call it at the top of a bot's message handler, before
// gateway or conversation processing.
func (e *Engine) WelcomeIfNeeded(ctx context.Context, id string) {
	w := e.Welcome
	if !w.Enabled || w.Text == "" || w.StateDir == "" {
		return
	}

	marker := filepath.Join(w.StateDir, "welcomed-"+sanitizeID(id))
	if _, err := os.Stat(marker); err == nil {
		return // already welcomed
	}

	if err := os.MkdirAll(w.StateDir, 0755); err != nil {
		slog.Warn(e.Label+": welcome state dir", "error", err)
		return
	}
	if err := os.WriteFile(marker, []byte{}, 0644); err != nil {
		slog.Warn(e.Label+": welcome marker", "error", err)
		return
	}

	slog.Info(e.Label+": first contact, sending welcome", "id", id)
	e.Sender.Final(ctx, id, w.Text)
}

// sanitizeID makes a conversation id safe for use as a filename.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
