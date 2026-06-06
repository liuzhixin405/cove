package api

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type MessagePart struct {
	Type     string `json:"type"` // text | image | file
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"` // base64 payload for image/file parts
	FileName string `json:"file_name,omitempty"`
}

type Message struct {
	Role             string        `json:"role"`
	Content          string        `json:"content,omitempty"`
	Parts            []MessagePart `json:"parts,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	Name             string        `json:"name,omitempty"`
	CacheControl     string        `json:"cache_control,omitempty"` // Anthropic prompt caching
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
	Content               string
	ReasoningContent      string
	ToolCalls             []ToolCall
	Model                 string
	InputTokens           int
	OutputTokens          int
	PromptCacheHitTokens  int
	PromptCacheMissTokens int
	ReasoningTokens       int
	StopReason            string
	RateLimitHeaders      http.Header // raw rate limit headers from response
}

type StreamEvent struct {
	Type      string    `json:"type"`
	Delta     string    `json:"delta,omitempty"`
	Reasoning string    `json:"reasoning,omitempty"`
	ToolCall  *ToolCall `json:"tool_call,omitempty"`
}

type StreamHandler func(event StreamEvent)

type Provider interface {
	Name() string
	DisplayName() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error)
	Validate() error
}

type ProviderConfig struct {
	Name    string
	APIKey  string
	APIKeys []string
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
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// sharedTransport is a single process-wide HTTP transport reused by all
// providers. http.Transport is safe for concurrent use and maintains its own
// connection pool, so sharing one instance avoids duplicate pools when multiple
// providers (or provider switches within a session) are created.
var (
	sharedTransportOnce sync.Once
	sharedTransport     *http.Transport
)

func defaultHTTPTransport() *http.Transport {
	sharedTransportOnce.Do(func() {
		sharedTransport = &http.Transport{
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 60 * time.Second,
			}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   20,
			MaxConnsPerHost:       30,
			IdleConnTimeout:       120 * time.Second,
			ResponseHeaderTimeout: 180 * time.Second,
			DisableCompression:    false,
			ForceAttemptHTTP2:     true,
		}
	})
	return sharedTransport
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

var defaultRetry = retryConfig{MaxRetries: 3, BaseDelay: 1 * time.Second}
