package api

import (
	"context"
	"time"
)

type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type ChatRequest struct {
	Model      string
	Messages   []Message
	System     string // additional system content appended after base
	SystemBase string // base system prompt (Anthropic: system field; OpenAI: first system message)
	Tools      []ToolDef
	MaxTokens  int
}

type ChatResponse struct {
	Content      string
	ToolCalls    []ToolCall
	Model        string
	InputTokens  int
	OutputTokens int
	StopReason   string
}

type StreamEvent struct {
	Type     string    `json:"type"`
	Delta    string    `json:"delta,omitempty"`
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

type StreamHandler func(event StreamEvent)

type Provider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error)
	Validate() error
}

type ProviderConfig struct {
	Name    string
	APIKey  string
	BaseURL string
}

func NewProvider(cfg ProviderConfig) Provider {
	cfg.Name = NormalizeProviderName(cfg.Name)
	if IsOpenAICompatibleProvider(cfg.Name) {
		return newOpenAICompatProvider(cfg)
	}
	return newAnthropicProvider(cfg)
}

func DetectProvider(model string, cfg ProviderConfig) Provider {
	if cfg.Name != "" {
		return NewProvider(cfg)
	}
	if containsAny(model, "deepseek") {
		cfg.Name = "deepseek"
		return newOpenAICompatProvider(cfg)
	}
	if containsAny(model, "gpt-", "o1-", "o3-", "o4-") {
		cfg.Name = "openai"
		return newOpenAICompatProvider(cfg)
	}
	cfg.Name = "anthropic"
	return newAnthropicProvider(cfg)
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

type AgentRunResult struct {
	Output  string
	Cost    float64
	Steps   int
	Success bool
	Error   string
}

type AgentRunner interface {
	Run(ctx context.Context, name string, task string) (*AgentRunResult, error)
	Register(name, description, prompt string)
}

type retryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
}

var defaultRetry = retryConfig{MaxRetries: 3, BaseDelay: time.Second}
