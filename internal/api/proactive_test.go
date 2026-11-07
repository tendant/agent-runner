package api

import (
	"context"
	"strings"
	"testing"
)

func TestProactiveChatRouting(t *testing.T) {
	c := proactiveChat{}
	for target, want := range map[string]string{
		"telegram": "Telegram bot is not configured",
		"c_1":      "agent-stream bot is not configured",
		"general":  "neither an agent-stream channel id",
	} {
		err := c.OpenProactiveThread(context.Background(), target, "r", "m", false)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", target, err, want)
		}
	}
}
