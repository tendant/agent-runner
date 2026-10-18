package stream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// On agent-stream anyone in a channel can address the bot: only its admins
// run chat commands, a leading mention is stripped from theirs, and another
// bot's command is ignored.
func TestStreamBot_CommandsOnlyFromAdmins(t *testing.T) {
	gw := &blockingGateway{release: make(chan struct{})}
	close(gw.release)
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.stateDir = t.TempDir()
	bot.admins = adminSet([]string{"u_owner"})

	seq := int64(0)
	send := func(id, user, kind, content string) {
		seq++
		p, _ := json.Marshal(messagePayload{MessageID: id, UserID: user, SenderKind: kind, Content: content, Addressees: []string{testBotUserID}})
		bot.processEventBatch(context.Background(), "c_1", []Event{{Seq: seq, Type: "message.created", Payload: p}}, seq-1)
		bot.wg.Wait()
	}
	send("m_1", "u_guest", "human", "/set ANTHROPIC_BASE_URL https://evil")
	send("m_2", "u_guest", "human", "@Agent /secret list")
	send("m_3", "u_otherbot", "bot", "/set PATH /tmp/evil")
	send("m_4", "u_owner", "human", "@Agent /status")
	send("m_5", "u_owner", "human", "/config")

	got := gw.handled()
	if len(got) != 2 || got[0] != "/status" || got[1] != "/config" {
		t.Fatalf("gateway handled %q, want only the owner's /status and /config", got)
	}
	for _, id := range []string{"m_1", "m_2", "m_3"} {
		if conv, ok := bot.threadManager.Get(id); ok && strings.Contains(conv.GetUserMessage(), "/") {
			t.Errorf("%s: a refused command entered the thread: %q", id, conv.GetUserMessage())
		}
	}
}

func TestStripLeadingMention(t *testing.T) {
	for in, want := range map[string]string{
		"@Agent /status":                 "/status",
		"@Lei Wang /secret list":         "/secret list",
		"@Agent please check /etc/hosts": "@Agent please check /etc/hosts",
		"/status":                        "/status",
		"hello":                          "hello",
	} {
		if got := stripLeadingMention(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
