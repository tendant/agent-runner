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

const defaultOpenAIBaseURL = "https://api.openai.com"

// OpenAIClient calls any OpenAI-compatible chat completions API.
// Works with OpenAI, Azure OpenAI, Ollama, and other compatible endpoints.
type OpenAIClient struct {
	apiKey     string
	model      string
	baseURL    string
	maxTokens  int
	httpClient *http.Client
}

func NewOpenAIClient(apiKey, model, baseURL string) *OpenAIClient {
	return NewOpenAIClientWithTokens(apiKey, model, baseURL, 512)
}

func NewOpenAIClientWithTokens(apiKey, model, baseURL string, maxTokens int) *OpenAIClient {
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	if maxTokens <= 0 {
		maxTokens = 512
	}
	return &OpenAIClient{
		apiKey:     apiKey,
		model:      model,
		baseURL:    baseURL,
		maxTokens:  maxTokens,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *OpenAIClient) Complete(ctx context.Context, prompt string) (string, error) {
	return c.CompleteWithImages(ctx, prompt, nil)
}

func (c *OpenAIClient) CompleteWithImages(ctx context.Context, prompt string, imagePaths []string) (string, error) {
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
		dataURI := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]string{"url": dataURI},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})

	payload := map[string]any{
		"model":      c.model,
		"max_tokens": c.maxTokens,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("openai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("openai: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("openai: parse response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices in response")
	}
	return result.Choices[0].Message.Content, nil
}
