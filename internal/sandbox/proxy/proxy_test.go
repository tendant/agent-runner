package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func setup(t *testing.T, mod func(*Config)) (*Proxy, *httptest.Server, *[]http.Header) {
	var seen []http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		io.WriteString(w, strings.Repeat("r", 100))
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	cfg := Config{Upstream: u, AuthHeader: "x-api-key", AuthValue: "REAL-PROVIDER-KEY", PathPrefixes: []string{"/v1/"}}
	if mod != nil {
		mod(&cfg)
	}
	p, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, up, &seen
}

func do(p *Proxy, path, token, body string) (*http.Response, string) {
	req, _ := http.NewRequest("POST", p.BaseURL()+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestInjectsCredentialAndHidesIt(t *testing.T) {
	p, _, seen := setup(t, nil)
	resp, body := do(p, "/v1/messages", p.Token(), "{}")
	if resp.StatusCode != 200 || len(body) != 100 {
		t.Fatalf("%d %d", resp.StatusCode, len(body))
	}
	h := (*seen)[0]
	if h.Get("X-Api-Key") != "REAL-PROVIDER-KEY" || h.Get("Authorization") != "" {
		t.Errorf("upstream headers: %v", h)
	}
	if strings.Contains(p.BaseURL(), "REAL") || strings.Contains(p.Token(), "REAL") {
		t.Error("secret leaked to sandbox-visible values")
	}
}

func TestRejectsUnauthorizedAndPaths(t *testing.T) {
	var events []string
	p, _, seen := setup(t, func(c *Config) { c.OnEvent = func(k string) { events = append(events, k) } })
	if r, _ := do(p, "/v1/x", "wrong", ""); r.StatusCode != 401 {
		t.Errorf("wrong token: %d", r.StatusCode)
	}
	if r, _ := do(p, "/v1/x", "", ""); r.StatusCode != 401 {
		t.Errorf("no token: %d", r.StatusCode)
	}
	if r, _ := do(p, "/other", p.Token(), ""); r.StatusCode != 403 {
		t.Errorf("path: %d", r.StatusCode)
	}
	if len(*seen) != 0 {
		t.Error("denied requests must not reach upstream")
	}
}

func TestLimits(t *testing.T) {
	p, _, _ := setup(t, func(c *Config) { c.MaxRequestB = 10 })
	if r, _ := do(p, "/v1/x", p.Token(), strings.Repeat("a", 50)); r.StatusCode != 413 {
		t.Errorf("big request: %d", r.StatusCode)
	}
	p, _, _ = setup(t, func(c *Config) { c.RatePerMinute = 2 })
	var codes []int
	for i := 0; i < 3; i++ {
		r, _ := do(p, "/v1/x", p.Token(), "")
		codes = append(codes, r.StatusCode)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Errorf("rate: %v", codes)
	}
	p, _, _ = setup(t, func(c *Config) { c.MaxTotalB = 150 })
	r1, b1 := do(p, "/v1/x", p.Token(), "")
	_, b2 := do(p, "/v1/x", p.Token(), "")
	r3, _ := do(p, "/v1/x", p.Token(), "")
	if r1.StatusCode != 200 || len(b1) != 100 || len(b2) >= 100 || r3.StatusCode != 429 {
		t.Errorf("budget: %d %d %d %d", r1.StatusCode, len(b1), len(b2), r3.StatusCode)
	}
}
