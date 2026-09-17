// Package callback delivers terminal session results to a caller-supplied
// webhook URL, so API clients can be told when a session ends instead of
// polling GET /agent/{id} or holding an SSE connection open.
package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Dispatcher POSTs JSON payloads to webhook URLs with bounded retries.
type Dispatcher struct {
	client   *http.Client
	attempts int
	backoff  func(attempt int) time.Duration
}

// New returns a Dispatcher with a 15s per-attempt timeout and three attempts
// backing off 1s, 4s, 16s — long enough to ride out a receiver restart,
// short enough that a dead URL doesn't pin a goroutine for long.
func New() *Dispatcher {
	return &Dispatcher{
		client:   &http.Client{Timeout: 15 * time.Second},
		attempts: 3,
		backoff: func(attempt int) time.Duration {
			return time.Duration(1<<(2*attempt)) * time.Second // 1s, 4s, 16s
		},
	}
}

// ValidateURL rejects anything that isn't an absolute http(s) URL with a host.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid callback_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid callback_url: scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("invalid callback_url: missing host")
	}
	return nil
}

// Deliver POSTs payload to target in the background. Failures are logged,
// never returned: a session's outcome doesn't depend on whether the caller
// is listening.
func (d *Dispatcher) Deliver(sessionID, target string, payload any) {
	if d == nil || target == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("callback: marshal failed", "session_id", sessionID, "error", err)
		return
	}
	go d.deliver(sessionID, target, body)
}

// DeliverSync is Deliver without the goroutine; tests and shutdown paths
// use it to wait for the outcome.
func (d *Dispatcher) DeliverSync(sessionID, target string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return d.deliver(sessionID, target, body)
}

func (d *Dispatcher) deliver(sessionID, target string, body []byte) error {
	var lastErr error
	for attempt := 0; attempt < d.attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(d.backoff(attempt - 1))
		}
		lastErr = d.post(target, sessionID, body)
		if lastErr == nil {
			slog.Info("callback delivered", "session_id", sessionID, "url", target, "attempt", attempt+1)
			return nil
		}
		if _, permanent := lastErr.(*permanentError); permanent {
			break
		}
		slog.Warn("callback attempt failed", "session_id", sessionID, "url", target, "attempt", attempt+1, "error", lastErr)
	}
	slog.Error("callback: giving up", "session_id", sessionID, "url", target, "attempts", d.attempts, "error", lastErr)
	return lastErr
}

func (d *Dispatcher) post(target, sessionID string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), d.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agent-runner-callback")
	req.Header.Set("X-Agent-Session-ID", sessionID)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 2xx is delivered; 4xx (other than 408/429) is the receiver rejecting
	// the payload and won't improve with retries; everything else retries.
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests:
		return &permanentError{code: resp.StatusCode}
	default:
		return fmt.Errorf("callback returned %d", resp.StatusCode)
	}
}

// permanentError short-circuits the retry loop.
type permanentError struct{ code int }

func (e *permanentError) Error() string { return fmt.Sprintf("callback rejected with %d", e.code) }
