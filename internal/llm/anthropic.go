package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const defaultAnthropicBaseURL = "https://api.anthropic.com"

// AnthropicClient calls the Anthropic Messages API directly.
type AnthropicClient struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
}

func NewAnthropicClient(apiKey, model, baseURL string) *AnthropicClient {
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}
	return &AnthropicClient{
		apiKey:     apiKey,
		model:      model,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *AnthropicClient) Complete(ctx context.Context, prompt string) (string, error) {
	return c.CompleteWithImages(ctx, prompt, nil)
}

func (c *AnthropicClient) CompleteWithImages(ctx context.Context, prompt string, imagePaths []string) (string, error) {
	// Build content blocks: images first, then text.
	var content []any
	for _, path := range imagePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable images
		}
		mediaType := "image/jpeg"
		if len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 {
			mediaType = "image/png"
		} else if len(data) >= 12 && string(data[8:12]) == "WEBP" {
			mediaType = "image/webp"
		}
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})

	payload := map[string]any{
		"model":      c.model,
		"max_tokens": 512,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("anthropic: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("anthropic: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("anthropic: parse response: %w", err)
	}
	for _, block := range result.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("anthropic: no text content in response")
}
