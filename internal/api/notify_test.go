package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
