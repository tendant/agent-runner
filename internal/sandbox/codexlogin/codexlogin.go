// Package codexlogin holds a codex ChatGPT login (an auth.json written by
// `codex login`) on the runner's side, so sandboxed codex can use a ChatGPT
// subscription through the model proxy without seeing a token.
//
// The file can be the user's own ~/.codex/auth.json, shared with codex on the
// host. The refresh token rotates, and codex shares one file between
// processes by re-reading it before a refresh and adopting tokens another
// process wrote; there is no file lock. This package plays by the same rule:
// it re-reads the file on every call, writes it atomically keeping every
// field, refreshes 10 minutes before expiry (codex waits until 5, so when both
// are active the runner refreshes first and codex finds fresh tokens), and
// after a failed refresh adopts tokens another process rotated meanwhile.
// Both refreshing in the same instant still fails one side; a login made for
// the runner alone avoids that.
package codexlogin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Codex's OAuth client (codex-rs login/src/auth/manager.rs, 0.149).
const (
	DefaultTokenURL = "https://auth.openai.com/oauth/token"
	clientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	refreshWindow   = 10 * time.Minute // codex: 5
	refreshInterval = 8 * 24 * time.Hour
)

// Login is one auth.json. Safe for concurrent use; it re-reads the file on
// every call, so a new `codex login` takes effect without a restart.
type Login struct {
	Path     string
	TokenURL string       // default DefaultTokenURL
	HTTP     *http.Client // default: 30s timeout
	Now      func() time.Time

	mu sync.Mutex
}

// Headers returns the headers a ChatGPT-mode codex request carries:
// the bearer access token and the account id. It refreshes first when due.
func (l *Login) Headers(ctx context.Context) (http.Header, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := l.load()
	if err != nil {
		return nil, err
	}
	if l.due(f) {
		used := f.tokens.RefreshToken
		if err := l.refresh(ctx, f); err != nil {
			// Lost a race? Another process (codex on the host) may have
			// rotated the token and written the file meanwhile.
			g, lerr := l.load()
			if lerr != nil || g.tokens.RefreshToken == used || l.expired(g) {
				return nil, err
			}
			f = g
		}
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+f.tokens.AccessToken)
	if f.tokens.AccountID != "" {
		h.Set("ChatGPT-Account-ID", f.tokens.AccountID)
	}
	return h, nil
}

// Check reports whether the file is a usable ChatGPT login, without refreshing.
func (l *Login) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.load()
	return err
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
}

// file keeps every field of auth.json, so a rewrite loses nothing codex wrote.
type file struct {
	raw    map[string]json.RawMessage
	tokens tokens
	rawTok map[string]json.RawMessage
	last   time.Time
}

func (l *Login) load() (*file, error) {
	b, err := os.ReadFile(l.Path)
	if err != nil {
		return nil, fmt.Errorf("codex login %s: %w (create it with: CODEX_HOME=%s codex login --device-auth)", l.Path, err, filepath.Dir(l.Path))
	}
	f := &file{}
	if err := json.Unmarshal(b, &f.raw); err != nil {
		return nil, fmt.Errorf("codex login %s: not JSON", l.Path)
	}
	var mode string
	_ = json.Unmarshal(f.raw["auth_mode"], &mode)
	if mode != "chatgpt" || f.raw["tokens"] == nil {
		return nil, fmt.Errorf("codex login %s: not a ChatGPT login (auth_mode %q)", l.Path, mode)
	}
	if err := json.Unmarshal(f.raw["tokens"], &f.tokens); err != nil {
		return nil, fmt.Errorf("codex login %s: bad tokens", l.Path)
	}
	_ = json.Unmarshal(f.raw["tokens"], &f.rawTok)
	if f.tokens.AccessToken == "" || f.tokens.RefreshToken == "" {
		return nil, fmt.Errorf("codex login %s: no access or refresh token", l.Path)
	}
	var last string
	if json.Unmarshal(f.raw["last_refresh"], &last) == nil {
		f.last, _ = time.Parse(time.RFC3339Nano, last)
	}
	return f, nil
}

func (l *Login) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// expired reports whether the access token is past its exp (unknown: no).
func (l *Login) expired(f *file) bool {
	exp, ok := jwtExp(f.tokens.AccessToken)
	return ok && !exp.After(l.now())
}

func (l *Login) due(f *file) bool {
	if exp, ok := jwtExp(f.tokens.AccessToken); ok {
		return !exp.After(l.now().Add(refreshWindow))
	}
	return !f.last.IsZero() && f.last.Before(l.now().Add(-refreshInterval))
}

// jwtExp reads a JWT's exp claim without verifying it (codex does the same).
func jwtExp(tok string) (time.Time, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp == nil {
		return time.Time{}, false
	}
	return time.Unix(int64(*c.Exp), 0), true
}

func (l *Login) refresh(ctx context.Context, f *file) error {
	body, _ := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": f.tokens.RefreshToken})
	u := l.TokenURL
	if u == "" {
		u = DefaultTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := l.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("codex login refresh: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Only the error code: the body may echo a token.
		var e struct {
			Error any `json:"error"`
		}
		_ = json.Unmarshal(rb, &e)
		code := ""
		switch v := e.Error.(type) {
		case string:
			code = v
		case map[string]any:
			code, _ = v["code"].(string)
		}
		return fmt.Errorf("codex login refresh: HTTP %d %s; sign in again with: CODEX_HOME=%s codex login --device-auth",
			resp.StatusCode, code, filepath.Dir(l.Path))
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(rb, &r); err != nil || r.AccessToken == "" {
		return errors.New("codex login refresh: no access token in the response")
	}
	set := func(k, v string) {
		if v != "" {
			b, _ := json.Marshal(v)
			f.rawTok[k] = b
		}
	}
	set("access_token", r.AccessToken)
	set("refresh_token", r.RefreshToken)
	set("id_token", r.IDToken)
	f.tokens.AccessToken = r.AccessToken
	if r.RefreshToken != "" {
		f.tokens.RefreshToken = r.RefreshToken
	}
	f.raw["tokens"], _ = json.Marshal(f.rawTok)
	f.raw["last_refresh"], _ = json.Marshal(l.now().UTC().Format(time.RFC3339Nano))
	return l.save(f)
}

// save writes atomically: a crash mid-write must not lose the rotated token.
func (l *Login) save(f *file) error {
	b, err := json.MarshalIndent(f.raw, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.Path), ".auth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.Path)
}
