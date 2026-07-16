package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/config"
)

// TestRecoverMiddleware_CatchesPanic verifies a panicking handler is turned
// into a clean 500 response instead of Go's default behavior of silently
// closing the connection (which surfaces to the client as a bare EOF with
// no server-side log trace).
func TestRecoverMiddleware_CatchesPanic(t *testing.T) {
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	srv := httptest.NewServer(recoverMiddleware(panicky))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("request failed (panic leaked past middleware): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

// TestRecoverMiddleware_PassesThroughNormalRequests confirms the middleware
// doesn't interfere with handlers that don't panic.
func TestRecoverMiddleware_PassesThroughNormalRequests(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("fine"))
	})

	srv := httptest.NewServer(recoverMiddleware(ok))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
}

// TestWriteTimeout_ExceedsAnalyzerTimeout confirms the server's WriteTimeout
// always leaves room above the analyzer's own per-call timeout — the bug
// this guards against: an analyzer call running longer than WriteTimeout
// gets its response silently dropped (bare EOF client-side, no server log).
func TestWriteTimeout_ExceedsAnalyzerTimeout(t *testing.T) {
	cases := []struct {
		name             string
		analyzerTimeout  int
		wantWriteTimeout time.Duration
	}{
		{"default 30s analyzer timeout", 30, 60 * time.Second},
		{"zero analyzer timeout uses floor", 0, 60 * time.Second},
		{"slow local model 90s analyzer timeout", 90, 120 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Analyzer: config.AnalyzerConfig{TimeoutSeconds: tc.analyzerTimeout}}
			got := writeTimeout(cfg)
			if got != tc.wantWriteTimeout {
				t.Errorf("writeTimeout() = %v, want %v", got, tc.wantWriteTimeout)
			}
			analyzerTimeout := time.Duration(tc.analyzerTimeout) * time.Second
			if got <= analyzerTimeout {
				t.Errorf("writeTimeout() = %v must exceed analyzer timeout %v", got, analyzerTimeout)
			}
		})
	}
}
