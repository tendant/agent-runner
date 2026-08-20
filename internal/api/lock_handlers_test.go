package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/locks"
)

func newLockHandlers() *Handlers {
	return &Handlers{lockManager: locks.NewManager()}
}

func postLock(t *testing.T, h *Handlers, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/lock", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.HandleLock(rec, req)
	return rec
}

func TestHandleLock_AcquireAndConflict(t *testing.T) {
	h := newLockHandlers()

	rec := postLock(t, h, `{"name":"sites-config","ttl_seconds":60}`, map[string]string{"X-Session-ID": "agent-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first acquire status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var ok map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok["holder"] != "agent-1" {
		t.Errorf("holder = %v, want agent-1", ok["holder"])
	}

	rec = postLock(t, h, `{"name":"sites-config","ttl_seconds":60}`, map[string]string{"X-Session-ID": "agent-2"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second acquire status = %d, want 409", rec.Code)
	}
	var conflict map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if conflict["held_by"] != "agent-1" {
		t.Errorf("held_by = %v, want agent-1", conflict["held_by"])
	}
}

func TestHandleLock_HolderFromBodyOverridesHeader(t *testing.T) {
	h := newLockHandlers()
	rec := postLock(t, h, `{"name":"n","holder":"explicit"}`, map[string]string{"X-Session-ID": "from-header"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got) //nolint:errcheck
	if got["holder"] != "explicit" {
		t.Errorf("holder = %v, want explicit", got["holder"])
	}
}

func TestHandleLock_RequiresHolder(t *testing.T) {
	h := newLockHandlers()
	rec := postLock(t, h, `{"name":"n"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when no holder is supplied", rec.Code)
	}
}

func TestHandleLock_RequiresName(t *testing.T) {
	h := newLockHandlers()
	rec := postLock(t, h, `{}`, map[string]string{"X-Session-ID": "agent-1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when no name is supplied", rec.Code)
	}
}

func TestHandleUnlock(t *testing.T) {
	h := newLockHandlers()
	postLock(t, h, `{"name":"sites-config"}`, map[string]string{"X-Session-ID": "agent-1"})

	// A different session must not be able to release it.
	req := httptest.NewRequest(http.MethodDelete, "/lock/sites-config", nil)
	req.Header.Set("X-Session-ID", "agent-2")
	rec := httptest.NewRecorder()
	h.HandleUnlock(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("release by non-holder status = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/lock/sites-config", nil)
	req.Header.Set("X-Session-ID", "agent-1")
	rec = httptest.NewRecorder()
	h.HandleUnlock(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("release by holder status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	// Now free.
	if rec := postLock(t, h, `{"name":"sites-config"}`, map[string]string{"X-Session-ID": "agent-2"}); rec.Code != http.StatusOK {
		t.Errorf("re-acquire after release status = %d, want 200", rec.Code)
	}
}

func TestHandleListLocks(t *testing.T) {
	h := newLockHandlers()
	postLock(t, h, `{"name":"a"}`, map[string]string{"X-Session-ID": "agent-1"})
	postLock(t, h, `{"name":"b"}`, map[string]string{"X-Session-ID": "agent-2"})

	req := httptest.NewRequest(http.MethodGet, "/locks", nil)
	rec := httptest.NewRecorder()
	h.HandleListLocks(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Locks []map[string]any `json:"locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Locks) != 2 {
		t.Errorf("listed %d locks, want 2", len(got.Locks))
	}
}

func TestHandleLock_MethodNotAllowed(t *testing.T) {
	h := newLockHandlers()
	req := httptest.NewRequest(http.MethodGet, "/lock", nil)
	rec := httptest.NewRecorder()
	h.HandleLock(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
