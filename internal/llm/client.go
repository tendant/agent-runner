// Package llm provides a minimal LLM client interface and implementations for
// making fast, single-turn completions. It is used by the conversation analyzer
// for intent routing before launching a full agent session.
package llm

import (
	"context"
	"log/slog"
	"os"

	"github.com/agent-runner/agent-runner/internal/executor"
)

// Client performs a single-turn text completion.
type Client interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// MultimodalClient extends Client with image support. Implementations that
// can process images (e.g. vision models) should implement this interface.
// imagePaths are absolute local file paths to include alongside the prompt.
type MultimodalClient interface {
	Client
	CompleteWithImages(ctx context.Context, prompt string, imagePaths []string) (string, error)
}

// Config holds configuration for building an LLM client.
type Config struct {
	Provider  string // "anthropic" | "openai" | "deepseek" | "" (auto-detect)
	Model     string // model ID; provider-specific default used if empty
	APIKey    string // falls back to provider-specific env var
	BaseURL   string // override API base URL (e.g. Ollama endpoint)
	MaxTokens int    // max tokens in response; 0 = provider default (512)
}

// NewClient builds a Client from cfg. When the configured provider has no API
// key, auto-detects from other available keys before falling back to
// ExecutorClient. Priority: configured provider → auto-detect → ExecutorClient.
// exec may be nil only if credentials are guaranteed to be present.
func NewClient(cfg Config, exec executor.Executor) Client {
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 512
	}

	// Try to build a client for the given provider+key.
	// Returns nil when no key is available for that provider.
	tryProvider := func(provider, model, apiKey, baseURL string) Client {
		switch provider {
		case "anthropic":
			if apiKey == "" {
				apiKey = os.Getenv("ANTHROPIC_API_KEY")
			}
			if apiKey == "" {
				return nil
			}
			if model == "" {
				model = "claude-haiku-4-5-20251001"
			}
			slog.Info("llm: using Anthropic client", "model", model)
			return NewAnthropicClientWithTokens(apiKey, model, baseURL, maxTokens)

		case "openai":
			if apiKey == "" {
				apiKey = os.Getenv("OPENAI_API_KEY")
			}
			if apiKey == "" && baseURL == "" {
				return nil
			}
			if model == "" {
				model = "gpt-4o-mini"
			}
			slog.Info("llm: using OpenAI client", "model", model, "base_url", baseURL)
			return NewOpenAIClientWithTokens(apiKey, model, baseURL, maxTokens)

		case "deepseek":
			if apiKey == "" {
				apiKey = os.Getenv("DEEPSEEK_API_KEY")
			}
			if apiKey == "" {
				return nil
			}
			if model == "" {
				model = "deepseek-chat"
			}
			if baseURL == "" {
				baseURL = "https://api.deepseek.com"
			}
			slog.Info("llm: using DeepSeek client", "model", model)
			return NewOpenAIClientWithTokens(apiKey, model, baseURL, maxTokens)
		}
		return nil
	}

	// 1. Honour explicitly configured provider (with its model and key).
	if cfg.Provider != "" {
		if c := tryProvider(cfg.Provider, cfg.Model, cfg.APIKey, cfg.BaseURL); c != nil {
			return c
		}
		slog.Warn("llm: configured provider has no API key, trying auto-detect", "provider", cfg.Provider)
	}

	// 2. Auto-detect from available API keys (anthropic → deepseek → openai).
	for _, p := range []string{"anthropic", "deepseek", "openai"} {
		if c := tryProvider(p, "", "", ""); c != nil {
			return c
		}
	}

	// 3. Last resort: executor CLI fallback.
	slog.Info("llm: no API credentials found, using executor fallback")
	return NewExecutorClient(exec)
}
