package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
