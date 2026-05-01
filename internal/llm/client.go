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

// NewClient builds a Client from cfg. Falls back to ExecutorClient (using exec)
// if no API credentials are available, preserving existing behavior.
// exec may be nil only if credentials are guaranteed to be present.
func NewClient(cfg Config, exec executor.Executor) Client {
	provider := cfg.Provider
	apiKey := cfg.APIKey

	// Auto-detect provider when not explicitly set.
	if provider == "" {
		if apiKey != "" {
			// Can't auto-detect from a bare key — default to Anthropic.
			provider = "anthropic"
		} else if os.Getenv("ANTHROPIC_API_KEY") != "" {
			provider = "anthropic"
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		} else if os.Getenv("OPENAI_API_KEY") != "" {
			provider = "openai"
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
	} else if apiKey == "" {
		// Provider explicitly set; fill key from well-known env vars.
		switch provider {
		case "anthropic":
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		case "openai":
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}

	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 512
	}

	switch provider {
	case "anthropic":
		if apiKey == "" {
			slog.Warn("llm: anthropic provider selected but no API key found, falling back to executor")
			break
		}
		model := cfg.Model
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		slog.Info("llm: using Anthropic client", "model", model)
		return NewAnthropicClientWithTokens(apiKey, model, cfg.BaseURL, maxTokens)

	case "openai":
		// API key is optional when a custom BaseURL is set (e.g. Ollama, local endpoints).
		if apiKey == "" && cfg.BaseURL == "" {
			slog.Warn("llm: openai provider selected but no API key found, falling back to executor")
			break
		}
		model := cfg.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		slog.Info("llm: using OpenAI client", "model", model, "base_url", cfg.BaseURL)
		return NewOpenAIClientWithTokens(apiKey, model, cfg.BaseURL, maxTokens)

	case "deepseek":
		if apiKey == "" {
			apiKey = os.Getenv("DEEPSEEK_API_KEY")
		}
		if apiKey == "" {
			slog.Warn("llm: deepseek provider selected but no API key found, falling back to executor")
			break
		}
		model := cfg.Model
		if model == "" {
			model = "deepseek-chat"
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = "https://api.deepseek.com"
		}
		slog.Info("llm: using DeepSeek client", "model", model)
		return NewOpenAIClientWithTokens(apiKey, model, baseURL, maxTokens)
	}

	// Fallback: use executor (Claude CLI).
	slog.Info("llm: no API credentials configured, using executor fallback")
	return NewExecutorClient(exec)
}
