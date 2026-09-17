package callback

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fastDispatcher() *Dispatcher {
	d := New()
	d.backoff = func(int) time.Duration { return time.Millisecond }
	return d
}

func TestDeliver_RetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("X-Agent-Session-ID") != "agent-1" {
			t.Errorf("missing session header")
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := fastDispatcher().DeliverSync("agent-1", srv.URL, map[string]any{"status": "failed"})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", calls.Load())
	}
	if got["status"] != "failed" {
		t.Errorf("payload not delivered: %v", got)
	}
}

func TestDeliver_GivesUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := fastDispatcher().DeliverSync("agent-1", srv.URL, map[string]any{}); err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", calls.Load())
	}
}

func TestDeliver_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := fastDispatcher().DeliverSync("agent-1", srv.URL, map[string]any{}); err == nil {
		t.Fatal("expected error on 404")
	}
	if calls.Load() != 1 {
		t.Errorf("4xx should not retry, got %d attempts", calls.Load())
	}
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{"http://localhost:9000/hook", "https://example.com/a?b=c"} {
		if err := ValidateURL(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ftp://x/y", "/relative", "http://", "not a url"} {
		if err := ValidateURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
