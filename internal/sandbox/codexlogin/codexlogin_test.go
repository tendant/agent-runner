package codexlogin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func jwt(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
}

func writeLogin(t *testing.T, access string) string {
	p := filepath.Join(t.TempDir(), "auth.json")
	b, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "last_refresh": "2026-10-01T00:00:00Z", "extra": "kept",
		"tokens": map[string]any{"access_token": access, "refresh_token": "refresh-1", "id_token": "id-1", "account_id": "acct-1", "other": 7},
	})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type tokenServer struct {
	calls  int
	got    map[string]string
	status int
	reply  map[string]any
	during func() // runs while the refresh is in flight
}

func (s *tokenServer) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		s.got = map[string]string{}
		json.NewDecoder(r.Body).Decode(&s.got)
		if s.during != nil {
			s.during()
		}
		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		json.NewEncoder(w).Encode(s.reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFreshTokenIsUsedAsIs(t *testing.T) {
	now := time.Now()
	access := jwt(now.Add(time.Hour))
	ts := &tokenServer{}
	l := &Login{Path: writeLogin(t, access), TokenURL: ts.start(t)}
	h, err := l.Headers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer "+access || h.Get("ChatGPT-Account-ID") != "acct-1" || ts.calls != 0 {
		t.Errorf("headers %v, refresh calls %d", h, ts.calls)
	}
}

func TestRefreshRotatesAndKeepsTheRest(t *testing.T) {
	now := time.Now()
	newAccess := jwt(now.Add(time.Hour))
	ts := &tokenServer{reply: map[string]any{"access_token": newAccess, "refresh_token": "refresh-2", "id_token": "id-2"}}
	path := writeLogin(t, jwt(now.Add(2*time.Minute))) // inside the 5-minute window
	l := &Login{Path: path, TokenURL: ts.start(t), Now: func() time.Time { return now }}
	h, err := l.Headers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ts.calls != 1 || ts.got["grant_type"] != "refresh_token" || ts.got["refresh_token"] != "refresh-1" || ts.got["client_id"] != clientID {
		t.Fatalf("refresh request %v (calls %d)", ts.got, ts.calls)
	}
	if h.Get("Authorization") != "Bearer "+newAccess {
		t.Errorf("not the new token: %v", h)
	}
	var f map[string]any
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &f)
	tok := f["tokens"].(map[string]any)
	if tok["refresh_token"] != "refresh-2" || tok["id_token"] != "id-2" || tok["account_id"] != "acct-1" || tok["other"] != float64(7) || f["extra"] != "kept" {
		t.Errorf("rewritten file: %s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	// The next call uses the saved token: no second refresh.
	if _, err := l.Headers(context.Background()); err != nil || ts.calls != 1 {
		t.Errorf("second call: %v, calls %d", err, ts.calls)
	}
}

func TestNoExpUsesLastRefreshAge(t *testing.T) {
	ts := &tokenServer{reply: map[string]any{"access_token": "opaque-2"}}
	path := writeLogin(t, "opaque-1") // not a JWT; last_refresh 2026-10-01
	u := ts.start(t)
	l := &Login{Path: path, TokenURL: u, Now: func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }}
	if h, _ := l.Headers(context.Background()); h.Get("Authorization") != "Bearer opaque-1" || ts.calls != 0 {
		t.Errorf("4 days old: %v, calls %d", h, ts.calls)
	}
	l.Now = func() time.Time { return time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC) }
	if h, _ := l.Headers(context.Background()); h.Get("Authorization") != "Bearer opaque-2" || ts.calls != 1 {
		t.Errorf("9 days old: %v, calls %d", h, ts.calls)
	}
}

func TestRefreshFailureNamesTheFixNotTheToken(t *testing.T) {
	ts := &tokenServer{status: 401, reply: map[string]any{"error": map[string]any{"code": "refresh_token_reused", "message": "refresh-1"}}}
	path := writeLogin(t, jwt(time.Now().Add(-time.Minute)))
	l := &Login{Path: path, TokenURL: ts.start(t)}
	_, err := l.Headers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refresh_token_reused") || !strings.Contains(err.Error(), "codex login --device-auth") || strings.Contains(err.Error(), "refresh-1") {
		t.Errorf("error: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "refresh-1") {
		t.Error("a failed refresh changed the file")
	}
}

func TestRejectsNonChatGPTLogins(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(p, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`), 0o600)
	if err := (&Login{Path: p}).Check(); err == nil || !strings.Contains(err.Error(), "not a ChatGPT login") {
		t.Errorf("api-key file: %v", err)
	}
	if err := (&Login{Path: filepath.Join(t.TempDir(), "none.json")}).Check(); err == nil || !strings.Contains(err.Error(), "codex login --device-auth") {
		t.Errorf("missing file: %v", err)
	}
}

func TestRefreshesBeforeCodexWould(t *testing.T) {
	now := time.Now()
	ts := &tokenServer{reply: map[string]any{"access_token": jwt(now.Add(time.Hour))}}
	// 8 minutes left: codex (5-minute window) would not refresh yet; we do.
	l := &Login{Path: writeLogin(t, jwt(now.Add(8*time.Minute))), TokenURL: ts.start(t), Now: func() time.Time { return now }}
	if _, err := l.Headers(context.Background()); err != nil || ts.calls != 1 {
		t.Errorf("err %v, refresh calls %d", err, ts.calls)
	}
}

// Shared with codex on the host: codex rotated the token while our refresh
// (with the now-used refresh token) was in flight.
func TestLostRefreshRaceAdoptsTheOtherProcesssTokens(t *testing.T) {
	now := time.Now()
	path := writeLogin(t, jwt(now.Add(3*time.Minute)))
	codexAccess := jwt(now.Add(time.Hour))
	ts := &tokenServer{status: 400, reply: map[string]any{"error": map[string]any{"code": "refresh_token_reused"}}}
	ts.during = func() {
		b, _ := os.ReadFile(path)
		var f map[string]any
		json.Unmarshal(b, &f)
		tok := f["tokens"].(map[string]any)
		tok["access_token"], tok["refresh_token"] = codexAccess, "refresh-codex"
		b, _ = json.Marshal(f)
		os.WriteFile(path, b, 0o600)
	}
	l := &Login{Path: path, TokenURL: ts.start(t), Now: func() time.Time { return now }}
	h, err := l.Headers(context.Background())
	if err != nil || h.Get("Authorization") != "Bearer "+codexAccess {
		t.Fatalf("got %v, %v; want codex's token", h, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "refresh-codex") {
		t.Error("codex's rotated token was overwritten")
	}
	// The next call refreshes nothing: codex's token is fresh.
	ts.calls = 0
	if _, err := l.Headers(context.Background()); err != nil || ts.calls != 0 {
		t.Errorf("next call: %v, refresh calls %d", err, ts.calls)
	}
}

func TestLostRaceWithAnExpiredTokenStillFails(t *testing.T) {
	now := time.Now()
	path := writeLogin(t, jwt(now.Add(-time.Minute)))
	ts := &tokenServer{status: 400, reply: map[string]any{"error": "invalid_grant"}}
	ts.during = func() { // rotated, but to a token that is already expired
		b, _ := os.ReadFile(path)
		s := strings.Replace(string(b), "refresh-1", "refresh-other", 1)
		os.WriteFile(path, []byte(s), 0o600)
	}
	l := &Login{Path: path, TokenURL: ts.start(t), Now: func() time.Time { return now }}
	if _, err := l.Headers(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err %v, want the refresh error", err)
	}
}
