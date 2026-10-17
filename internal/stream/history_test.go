package stream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// A thread's history from the server never carries chat commands or their
// replies to the agent: a /secret set or /set holds a secret's value.
func TestThreadHistory_LeavesOutChatCommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/threads/m_1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[
			{"message_id":"m_1","user_id":"u_h","sender_kind":"human","content":"the export is broken"},
			{"message_id":"m_2","user_id":"u_h","sender_kind":"human","content":"/secret set GITEE_TOKEN tok-123456789 Gitee"},
			{"message_id":"m_3","user_id":"u_bot","sender_kind":"bot","content":"ok saved GITEE_TOKEN"},
			{"message_id":"m_4","user_id":"u_h","sender_kind":"human","content":" /set ANTHROPIC_API_KEY sk-ant-987654321"},
			{"message_id":"m_5","user_id":"u_bot","sender_kind":"bot","content":"ok set"},
			{"message_id":"m_6","user_id":"u_bot","sender_kind":"bot","content":"earlier answer"},
			{"message_id":"m_7","user_id":"u_h","sender_kind":"human","content":"please fix it"}
		]`))
	}))
	t.Cleanup(srv.Close)
	mgr := thread.NewManager("")
	t.Cleanup(mgr.Stop)
	bot := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "t"}, "", &trackingStarter{}, mgr, nil, nil)
	bot.botUserID = "u_bot"

	got, err := bot.threadHistory(context.Background(), "m_1")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, m := range got {
		all = append(all, m.Speaker+": "+m.Content)
	}
	joined := strings.Join(all, "\n")
	for _, leaked := range []string{"tok-123456789", "sk-ant-987654321", "/secret", "/set", "ok saved", "ok set"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("history carries %q:\n%s", leaked, joined)
		}
	}
	for _, kept := range []string{"the export is broken", "earlier answer", "please fix it"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("history lost %q:\n%s", kept, joined)
		}
	}
}
