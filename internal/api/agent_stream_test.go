package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// A busy iteration (many tool calls, each a session update) must announce
// itself once, and again only when its plan progress moves — agent-cli
// prints a line per iteration_start.
func TestStreamAnnouncesAnIterationOnce(t *testing.T) {
	m := agent.NewManager(3600, 10, 1)
	t.Cleanup(m.Stop)
	s, _ := m.CreateSession("hi", nil, "", "", 1, 0)
	h := &Handlers{agentManager: m}
	srv := httptest.NewServer(http.HandlerFunc(h.HandleStreamAgent))
	t.Cleanup(srv.Close)

	go func() {
		pause := func() { time.Sleep(30 * time.Millisecond) } // one notification each
		s.BeginIteration(1)
		pause()
		for i := 0; i < 5; i++ {
			s.AppendExecEvent("tool_start", "Bash", time.Now())
			pause()
		}
		s.SetCompletedSteps([]string{"step 1"})
		s.AppendExecEvent("tool_end", "Bash", time.Now())
		pause()
		for i := 0; i < 3; i++ {
			s.AppendExecEvent("tool_start", "Read", time.Now())
			pause()
		}
		s.Complete()
	}()

	resp, err := http.Get(srv.URL + "/agent/" + s.ID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if n := strings.Count(body, "event: iteration_start"); n != 2 {
		t.Errorf("iteration_start sent %d times, want 2 (start, then step progress):\n%s", n, body)
	}
	if n := strings.Count(body, "event: agent_event"); n != 9 {
		t.Errorf("agent_event sent %d times, want 9", n)
	}
	if !strings.Contains(body, "event: done") {
		t.Error("no done event")
	}
}
