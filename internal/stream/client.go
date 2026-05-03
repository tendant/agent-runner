package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
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

// DownloadedFile holds the result of downloading a file.
type DownloadedFile struct {
	Data        []byte
	Filename    string
	ContentType string
}

// DownloadFile downloads a file by ID, returning its content, filename, and content type.
func (c *Client) DownloadFile(ctx context.Context, fileID string) (*DownloadedFile, error) {
	url := fmt.Sprintf("%s/v1/files/%s", c.serverURL, fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("download file: status %d: %s", resp.StatusCode, string(body))
	}

	// Limit download to 10MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read file body: %w", err)
	}

	// Extract filename from Content-Disposition header
	filename := fileID
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := params["filename"]; fn != "" {
				filename = fn
			}
		}
	}

	return &DownloadedFile{
		Data:        data,
		Filename:    filename,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// UploadFile uploads a file to a conversation and returns the file ID.
func (c *Client) UploadFile(ctx context.Context, conversationID, filename, contentType string, data []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write file data: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close multipart: %w", err)
	}

	url := fmt.Sprintf("%s/v1/conversations/%s/files", c.serverURL, conversationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload file: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID string `json:"file_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}

	return result.ID, nil
}

// SendMessage sends a message to a conversation, optionally with file attachments.
func (c *Client) SendMessage(ctx context.Context, conversationID, content string, fileIDs []string) error {
	body := map[string]interface{}{
		"content": content,
	}
	if len(fileIDs) > 0 {
		body["file_ids"] = fileIDs
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	url := fmt.Sprintf("%s/v1/conversations/%s/messages", c.serverURL, conversationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send message: status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// ErrNotFound is returned by PollEvents when the server responds with 404.
// Callers can use errors.Is to detect this and fall back to SSE catch-up.
var ErrNotFound = fmt.Errorf("not found")

// PollEvents fetches events after afterSeq in a single HTTP request (no SSE required).
// Returns all new events sorted by seq. Suitable for polling loops.
// Returns ErrNotFound if the server responds with 404 (endpoint not supported).
func (c *Client) PollEvents(ctx context.Context, conversationID string, afterSeq int64) ([]Event, error) {
	url := fmt.Sprintf("%s/v1/conversations/%s/events?after_seq=%d", c.serverURL, conversationID, afterSeq)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll events: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("poll events: status %d: %s", resp.StatusCode, string(body))
	}

	var events []Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("poll events: decode: %w", err)
	}
	return events, nil
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
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	// Use a separate client without timeout for SSE (long-lived connection).
	// Disable HTTP/2: SSE requires chunked streaming which HTTP/2 multiplexing
	// can interfere with on some server implementations.
	sseClient := &http.Client{
		Transport: &http.Transport{
			ForceAttemptHTTP2: false,
		},
	}
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
// Returns the number of events successfully delivered.
func (c *Client) readSSE(ctx context.Context, r io.Reader, ch chan<- Event) int {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024) // 1 MB max line
	var dataLines []string
	var count int

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return count
		default:
		}

		line := scanner.Text()

		if line == "" {
			// Empty line = end of event
			if len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				dataLines = nil

				var event Event
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					slog.Warn("SSE event parse error", "error", err, "data", data)
					continue
				}
				select {
				case ch <- event:
					count++
				case <-ctx.Done():
					return count
				}
			}
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("SSE scanner error", "error", err)
	}
	return count
}
