package stream

import (
	"context"
	"encoding/json"
	"testing"
)

// The bot acts only on Messages the server addressed to it; everything else
// in its channels — other threads, other bots, its own messages — is left
// alone.
func TestStreamBot_ActsOnlyWhenAddressed(t *testing.T) {
	gw := &blockingGateway{release: make(chan struct{})}
	close(gw.release)
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.stateDir = t.TempDir()
	bot.botUserID = "u_me"

	seq := int64(0)
	send := func(id, thread, user, kind, content string, addressees ...string) {
		seq++
		p, _ := json.Marshal(messagePayload{MessageID: id, ThreadID: thread, UserID: user, SenderKind: kind, Content: content, Addressees: addressees})
		bot.processEventBatch(context.Background(), "c_1", []Event{{Seq: seq, Type: "message.created", Payload: p}}, seq-1)
		bot.wg.Wait()
	}

	send("m_1", "", "u_human", "human", "for me", "u_me")
	send("m_2", "", "u_human", "human", "for the other bot", "u_other")
	send("m_3", "", "u_human", "human", "for nobody")
	send("m_4", "m_1", "u_other", "bot", "consulting me", "u_me")
	send("m_5", "m_1", "u_me", "bot", "my own result")
	send("m_6", "m_1", "u_human", "human", "for both", "u_other", "u_me")

	got := gw.handled()
	want := []string{"for me", "consulting me", "for both"}
	if len(got) != len(want) {
		t.Fatalf("handled %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("handled %q, want %q", got, want)
		}
	}
}

// Without a known user ID the bot can't tell what's addressed to it, so it
// acts on nothing.
func TestAddressedTo(t *testing.T) {
	msg := messagePayload{Addressees: []string{"u_me"}}
	if !addressedTo(msg, "u_me") || addressedTo(msg, "u_other") || addressedTo(msg, "") {
		t.Fatal("addressedTo misreads the addressees")
	}
}
