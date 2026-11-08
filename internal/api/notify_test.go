package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
)

type fakeNotifier struct{ err error }

func (f fakeNotifier) SendNotification(context.Context, string) error { return f.err }

func notify(t *testing.T, h *Handlers) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.HandleNotify(rec, httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(`{"message":"deploy done"}`)))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// /notify reports per backend: all sent, some sent (200 partial, naming the
// failure), none sent (502), or nowhere to send (503).
func TestHandleNotify(t *testing.T) {
	down := fakeNotifier{errors.New("connection reset")}

	h := &Handlers{}
	if code, _ := notify(t, h); code != http.StatusServiceUnavailable {
		t.Errorf("no notifier: %d", code)
	}
	h.SetNotifier(NewMultiNotifier(fakeNotifier{}, fakeNotifier{}))
	if code, out := notify(t, h); code != 200 || out["status"] != "sent" {
		t.Errorf("all sent: %d %v", code, out)
	}
	h.SetNotifier(NewMultiNotifier(fakeNotifier{}, down))
	code, out := notify(t, h)
	if code != 200 || out["status"] != "partial" || !strings.Contains(strings.Join(toStrings(out["failed"]), ""), "connection reset") {
		t.Errorf("partial: %d %v", code, out)
	}
	h.SetNotifier(NewMultiNotifier(down, down))
	if code, out := notify(t, h); code != http.StatusBadGateway {
		t.Errorf("none sent: %d %v", code, out)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Without API_KEY only requests to a loopback name pass: a web page that
// rebinds its own name to 127.0.0.1 is refused.
func TestLoopbackHostMiddleware(t *testing.T) {
	h := loopbackHostMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for host, want := range map[string]int{
		"127.0.0.1:8080": 200, "localhost:8080": 200, "[::1]:8080": 200,
		"attacker.example:8080": 403, "192.168.3.110:8080": 403, "evil.localhost.example": 403,
	} {
		req := httptest.NewRequest("POST", "/agent", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.Code, want)
		}
	}
}

type fileNotifier struct {
	files *[]string
}

func (f fileNotifier) SendNotification(context.Context, string) error { return nil }
func (f fileNotifier) SendNotificationFiles(_ context.Context, _ string, files []agent.OutputFile) error {
	for _, x := range files {
		*f.files = append(*f.files, x.Name)
	}
	return nil
}

type textNotifier struct{ got *string }

func (n textNotifier) SendNotification(_ context.Context, m string) error { *n.got = m; return nil }

// Files go to the backends that can attach them; the others get the message
// with the files named.
func TestMultiNotifierFiles(t *testing.T) {
	var attached []string
	var text string
	m := NewMultiNotifier(fileNotifier{&attached}, textNotifier{&text})
	files := []agent.OutputFile{{Name: "report.md", Data: []byte("x")}}
	if err := m.SendNotificationFiles(context.Background(), "done", files); err != nil {
		t.Fatal(err)
	}
	if len(attached) != 1 || attached[0] != "report.md" {
		t.Errorf("attached: %v", attached)
	}
	if !strings.HasPrefix(text, "done") || !strings.Contains(text, "report.md") {
		t.Errorf("text backend got %q", text)
	}
}
