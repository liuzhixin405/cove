// Package mobileapi provides the API types and provider for the mobile engine.
// This is a self-contained package that does not depend on internal packages,
// making it compatible with gomobile bind.
package mobileapi

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

// Message represents a chat message
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall represents an AI tool call
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolDef defines a tool available to the AI
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

// ChatRequest is the request to the chat API
type ChatRequest struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	SystemBase string    `json:"-"`
	System     string    `json:"-"`
	Tools      []ToolDef `json:"tools,omitempty"`
	MaxTokens  int       `json:"max_tokens,omitempty"`
}

// StreamEvent is a streaming response event
type StreamEvent struct {
	Delta     string
	Reasoning string
}

// ProviderConfig holds provider configuration
type ProviderConfig struct {
	Name    string
	APIKey  string
	BaseURL string
}

// Provider is the interface for AI providers
type Provider interface {
	ChatStream(ctx context.Context, req ChatRequest, onEvent func(StreamEvent)) (ChatResponse, error)
}

// ChatResponse is the non-streaming response
type ChatResponse struct {
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	InputTokens      int
	OutputTokens     int
}

// NewProvider creates a provider based on configuration
func NewProvider(cfg ProviderConfig) Provider {
	normalized := strings.ToLower(cfg.Name)
	switch normalized {
	case "anthropic":
		return newAnthropicProvider(cfg)
	default:
		return newOpenAICompatProvider(cfg)
	}
}

// DetectProvider detects the provider from model name and config
func DetectProvider(model string, cfg ProviderConfig) Provider {
	if cfg.Name != "" {
		return NewProvider(cfg)
	}
	if strings.Contains(strings.ToLower(model), "deepseek") {
		cfg.Name = "deepseek"
		return newOpenAICompatProvider(cfg)
	}
	if strings.Contains(strings.ToLower(model), "claude") {
		cfg.Name = "anthropic"
		return newAnthropicProvider(cfg)
	}
	if strings.Contains(strings.ToLower(model), "gpt") || strings.Contains(strings.ToLower(model), "o1") || strings.Contains(strings.ToLower(model), "o3") {
		cfg.Name = "openai"
		return newOpenAICompatProvider(cfg)
	}
	cfg.Name = "openai"
	return newOpenAICompatProvider(cfg)
}

// ---------- OpenAI Compatible Provider ----------

type openAICompatProvider struct {
	name    string
	apiKey  string
	baseURL string
}

func newOpenAICompatProvider(cfg ProviderConfig) *openAICompatProvider {
	p := &openAICompatProvider{
		name:   cfg.Name,
		apiKey: cfg.APIKey,
	}
	if cfg.BaseURL != "" {
		p.baseURL = strings.TrimRight(cfg.BaseURL, "/")
	} else {
		switch cfg.Name {
		case "deepseek":
			p.baseURL = "https://api.deepseek.com/v1"
		case "openai":
			p.baseURL = "https://api.openai.com/v1"
		default:
			p.baseURL = "https://api.deepseek.com/v1"
		}
	}
	return p
}

type oaiStreamChoice struct {
	Delta struct {
		Content   string        `json:"content"`
		Reasoning string        `json:"reasoning_content"`
		ToolCalls []oaiToolCall `json:"tool_calls"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type oaiStreamChunk struct {
	Choices []oaiStreamChoice `json:"choices"`
}

type oaiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiToolCall struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function oaiToolCallFunction `json:"function"`
}

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type oaiErrorBody struct {
	Error oaiError `json:"error"`
}

func (p *openAICompatProvider) ChatStream(ctx context.Context, req ChatRequest, onEvent func(StreamEvent)) (ChatResponse, error) {
	// Build messages as generic maps to support tool_calls and tool_call_id fields
	var messages []map[string]interface{}

	if req.SystemBase != "" || req.System != "" {
		combined := req.SystemBase
		if combined != "" && req.System != "" {
			combined += "\n\n"
		}
		combined += req.System
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": combined,
		})
	}

	for _, m := range req.Messages {
		// Handle tool call messages (assistant with tool_calls)
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]interface{}
			for _, tc := range m.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Input)
				tcs = append(tcs, map[string]interface{}{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      tc.Name,
						"arguments": string(argsJSON),
					},
				})
			}
			messages = append(messages, map[string]interface{}{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": tcs,
			})
			continue
		}

		// Handle tool result messages (role: "tool")
		if m.Role == "tool" {
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      m.Content,
			})
			continue
		}

		// Regular user/assistant messages
		messages = append(messages, map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		})
	}

	body := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
		"stream":   true,
	}

	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}

	if len(req.Tools) > 0 {
		var toolDefs []map[string]interface{}
		for _, td := range req.Tools {
			toolDefs = append(toolDefs, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  td.InputSchema,
				},
			})
		}
		body["tools"] = toolDefs
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("http request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		var errBody oaiErrorBody
		if json.Unmarshal(b, &errBody) == nil && errBody.Error.Message != "" {
			return ChatResponse{}, fmt.Errorf("API error %d: %s", httpResp.StatusCode, errBody.Error.Message)
		}
		return ChatResponse{}, fmt.Errorf("API error %d: %s", httpResp.StatusCode, string(b))
	}

	var resp ChatResponse
	var toolArgsAccum []string
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				resp.Content += choice.Delta.Content
				onEvent(StreamEvent{Delta: choice.Delta.Content})
			}
			if choice.Delta.Reasoning != "" {
				resp.ReasoningContent += choice.Delta.Reasoning
				onEvent(StreamEvent{Reasoning: choice.Delta.Reasoning})
			}
			// Handle tool calls from streaming chunks
			for _, tc := range choice.Delta.ToolCalls {
				// Use index to match chunks for the same tool call (streaming args)
				if tc.Index >= len(resp.ToolCalls) {
					// New tool call - extend slice
					for len(resp.ToolCalls) <= tc.Index {
						resp.ToolCalls = append(resp.ToolCalls, ToolCall{
							ID:    tc.ID,
							Name:  tc.Function.Name,
							Input: make(map[string]any),
						})
					}
					// Initialize the arguments accumulator
					if len(toolArgsAccum) <= tc.Index {
						for len(toolArgsAccum) <= tc.Index {
							toolArgsAccum = append(toolArgsAccum, "")
						}
					}
				}
				// Update ID and name if present (first chunk has them)
				if tc.ID != "" {
					resp.ToolCalls[tc.Index].ID = tc.ID
				}
				if tc.Function.Name != "" {
					resp.ToolCalls[tc.Index].Name = tc.Function.Name
				}
				// Accumulate raw arguments (streamed in pieces)
				if tc.Function.Arguments != "" {
					toolArgsAccum[tc.Index] += tc.Function.Arguments
				}
			}
		}
	}

	// Parse accumulated tool call arguments
	for i, raw := range toolArgsAccum {
		if raw != "" && i < len(resp.ToolCalls) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
				resp.ToolCalls[i].Input = parsed
			}
		}
	}

	return resp, scanner.Err()
}

// ---------- Anthropic Provider (stub) ----------

type anthropicProvider struct {
	apiKey  string
	baseURL string
}

func newAnthropicProvider(cfg ProviderConfig) *anthropicProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	return &anthropicProvider{apiKey: cfg.APIKey, baseURL: strings.TrimRight(baseURL, "/")}
}

func (p *anthropicProvider) ChatStream(ctx context.Context, req ChatRequest, onEvent func(StreamEvent)) (ChatResponse, error) {
	return ChatResponse{}, fmt.Errorf("Anthropic provider not yet supported on mobile")
}

