package stream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// A bot without STREAM_CHANNEL_IDS follows its memberships: it listens on
// every channel it is in and picks up joins and leaves while running.
func TestStreamBot_FollowsChannelMemberships(t *testing.T) {
	var mu sync.Mutex
	channels := []string{"c_a", "c_b"}
	polled := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v2/channels":
			var out []map[string]string
			for _, c := range channels {
				out = append(out, map[string]string{"channel_id": c})
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/events"):
			id := strings.Split(r.URL.Path, "/")[3]
			polled[id]++
			w.Write([]byte("[]"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	threadMgr := thread.NewManager("")
	t.Cleanup(threadMgr.Stop)
	bot := New(config.StreamConfig{
		ServerURL:         srv.URL,
		BotToken:          "test-token",
		PollInterval:      10 * time.Millisecond,
		DiscoveryInterval: 20 * time.Millisecond,
	}, "", &trackingStarter{}, threadMgr, nil, nil)
	bot.stateDir = t.TempDir()
	if err := bot.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bot.Stop()

	waitChannels := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Join(bot.activeChannels(), ",") == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("listening on %v, want %s", bot.activeChannels(), want)
	}
	waitChannels("c_a,c_b")

	// Added to c_c, removed from c_a.
	mu.Lock()
	channels = []string{"c_b", "c_c"}
	mu.Unlock()
	waitChannels("c_b,c_c")

	// c_a's listener has stopped polling.
	mu.Lock()
	before := polled["c_a"]
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	after, cPolled := polled["c_a"], polled["c_c"]
	mu.Unlock()
	if after != before {
		t.Errorf("left channel c_a still polled (%d → %d)", before, after)
	}
	if cPolled == 0 {
		t.Error("joined channel c_c never polled")
	}
}

// A failed listing leaves the current listeners alone.
func TestStreamBot_DiscoveryErrorKeepsListeners(t *testing.T) {
	var mu sync.Mutex
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v2/channels" {
			if fail {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Write([]byte(`[{"channel_id":"c_a"}]`))
			return
		}
		w.Write([]byte("[]"))
	}))
	defer srv.Close()
	threadMgr := thread.NewManager("")
	t.Cleanup(threadMgr.Stop)
	bot := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "t", PollInterval: 10 * time.Millisecond, DiscoveryInterval: 20 * time.Millisecond},
		"", &trackingStarter{}, threadMgr, nil, nil)
	bot.stateDir = t.TempDir()
	bot.Start(context.Background())
	defer bot.Stop()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	fail = true
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if got := bot.activeChannels(); len(got) != 1 || got[0] != "c_a" {
		t.Fatalf("listeners after a failed listing = %v, want [c_a]", got)
	}
}
