package stream

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAddresses(t *testing.T) {
	cases := []struct {
		text, name string
		want       bool
	}{
		{"@helper can you check the build?", "helper", true},
		{"thanks @Helper.", "helper", true},
		{"cc @helper", "helper", true},
		{"ping @helper-bot", "helper", false},
		{"@help me", "helper", false},
		{"helper: no at-sign", "helper", false},
		{"@helper2 and @helper", "helper", true},
		{"@Video Bot please render", "Video Bot", true},
		{"@助手 请看", "助手", true},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := addresses(c.text, c.name); got != c.want {
			t.Errorf("addresses(%q, %q) = %v, want %v", c.text, c.name, got, c.want)
		}
	}
}

// Another bot's message reaches the agent only when it addresses this bot,
// and bot-to-bot exchanges in a thread stop at the cap until a human
// speaks; human messages are always handled.
func TestStreamBot_AnswersOtherBotsOnlyWhenAddressed(t *testing.T) {
	gw := &blockingGateway{release: make(chan struct{})}
	close(gw.release)
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.stateDir = t.TempDir()
	bot.botUserID = "u_me"
	bot.setBotName("helper")

	seq := int64(0)
	send := func(id, thread, user, kind, content string) {
		seq++
		p, _ := json.Marshal(messagePayload{MessageID: id, ThreadID: thread, UserID: user, SenderKind: kind, Content: content})
		bot.processEventBatch(context.Background(), "c_1", []Event{{Seq: seq, Type: "message.created", Payload: p}}, seq-1)
		bot.wg.Wait()
	}

	send("m_1", "", "u_human", "human", "hello")
	send("m_2", "m_1", "u_other", "bot", "not for you")
	send("m_3", "m_1", "u_other", "bot", "@helper please review")
	send("m_4", "m_1", "u_me", "bot", "@helper my own message")
	got := gw.handled()
	if len(got) != 2 || got[0] != "hello" || got[1] != "@helper please review" {
		t.Fatalf("handled %q, want the human message and the addressed bot message", got)
	}

	// Cap: after maxBotTurnsPerThread addressed bot messages, more are ignored…
	for i := 0; i < maxBotTurnsPerThread+2; i++ {
		send("m_x", "m_1", "u_other", "bot", "@helper again")
	}
	if n := len(gw.handled()); n != 2+maxBotTurnsPerThread-1 {
		t.Fatalf("handled %d messages, want the cap to stop bot-to-bot turns at %d", n, maxBotTurnsPerThread)
	}
	// …until a human speaks in the thread.
	send("m_h", "m_1", "u_human", "human", "go on")
	send("m_y", "m_1", "u_other", "bot", "@helper once more")
	if last := gw.handled(); last[len(last)-1] != "@helper once more" {
		t.Fatalf("bot message after a human reset not handled: %q", last)
	}
}

// With no known name, no bot message addresses this bot.
func TestStreamBot_UnknownNameIgnoresBots(t *testing.T) {
	gw := &blockingGateway{release: make(chan struct{})}
	close(gw.release)
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.stateDir = t.TempDir()
	p, _ := json.Marshal(messagePayload{MessageID: "m_1", UserID: "u_other", SenderKind: "bot", Content: "@anyone hi"})
	bot.processEventBatch(context.Background(), "c_1", []Event{{Seq: 1, Type: "message.created", Payload: p}}, 0)
	bot.wg.Wait()
	if got := gw.handled(); len(got) != 0 {
		t.Fatalf("handled %q with no bot name", got)
	}
}
