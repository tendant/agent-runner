package stream

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// One failed channel listing (a dropped connection) is not worth a warning;
// failures in a row are, and so is the recovery after them.
func TestSyncChannels_WarnsOnlyOnRepeatedFailures(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "reset", http.StatusBadGateway)
			return
		}
		w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	mgr := thread.NewManager("")
	t.Cleanup(mgr.Stop)
	bot := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "t"}, "", &trackingStarter{}, mgr, nil, nil)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.Background()
	bot.syncChannels(ctx)
	bot.syncChannels(ctx)
	if strings.Contains(logs.String(), "could not list channels") {
		t.Fatalf("warned on the first failures:\n%s", logs.String())
	}
	bot.syncChannels(ctx)
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "failures_in_a_row=3") {
		t.Fatalf("no warning after 3 failures in a row:\n%s", logs.String())
	}
	fail.Store(false)
	bot.syncChannels(ctx)
	if !strings.Contains(logs.String(), "listing channels works again") || bot.listFailures != 0 {
		t.Fatalf("recovery not logged or counter not reset:\n%s", logs.String())
	}
}
