package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Event represents a server-sent event from agent-stream.
type Event struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Client is an HTTP + SSE client for the agent-stream API.
type Client struct {
	serverURL  string
	botToken   string
	httpClient *http.Client
}

// NewClient creates a new agent-stream API client.
func NewClient(serverURL, botToken string) *Client {
	return &Client{
		serverURL: strings.TrimRight(serverURL, "/"),
		botToken:  botToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// EmitEvent sends an event to a conversation.
func (c *Client) EmitEvent(ctx context.Context, conversationID, eventType string, payload json.RawMessage) error {
	body := map[string]interface{}{
		"type":    eventType,
		"payload": json.RawMessage(payload),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	url := fmt.Sprintf("%s/v1/conversations/%s/events", c.serverURL, conversationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("emit event: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("emit event: status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// StreamEvents opens an SSE connection and returns a channel of events.
// The channel is closed when the context is cancelled or the connection drops.
func (c *Client) StreamEvents(ctx context.Context, conversationID string, afterSeq int64) (<-chan Event, error) {
	url := fmt.Sprintf("%s/v1/conversations/%s/events/stream?after_seq=%d", c.serverURL, conversationID, afterSeq)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	// Use a separate client without timeout for SSE (long-lived connection)
	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SSE connect: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("SSE connect: status %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		c.readSSE(ctx, resp.Body, ch)
	}()

	return ch, nil
}

// readSSE parses SSE lines from the reader and sends parsed events to the channel.
func (c *Client) readSSE(ctx context.Context, r io.Reader, ch chan<- Event) {
	scanner := bufio.NewScanner(r)
	var dataLines []string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()

		if line == "" {
			// Empty line = end of event
			if len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				dataLines = nil

				var event Event
				if err := json.Unmarshal([]byte(data), &event); err == nil {
					select {
					case ch <- event:
					case <-ctx.Done():
						return
					}
				}
			}
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}
}
