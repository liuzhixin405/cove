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

// maxStreamToolCalls bounds how many tool calls one streamed response may
// declare, so a bogus index cannot drive an unbounded slice growth.
const maxStreamToolCalls = 256

// streamHTTPClient is shared by every streaming request.
//
// It deliberately has no Client.Timeout (that would cap the whole stream — see
// ChatStream) and instead bounds only the phases that can legitimately hang
// before data starts flowing. Sharing one client also restores connection
// keep-alive: a fresh http.Client per call opened a new TLS connection for
// every turn, which on a mobile network is the slowest part of the request.
var streamHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	},
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
	Error   *oaiError         `json:"error,omitempty"`
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
	// any, not string: some servers send a numeric code, and a type
	// mismatch failed the whole chunk's decode, hiding the error.
	Code any `json:"code"`
}

type oaiErrorBody struct {
	Error oaiError `json:"error"`
}

func (p *openAICompatProvider) ChatStream(ctx context.Context, req ChatRequest, onEvent func(StreamEvent)) (ChatResponse, error) {
	// onEvent is optional. This is a mobile-binding entry point, so a caller
	// that only wants the final response passes nil — which used to panic on
	// the first delta. Substitute a no-op instead of guarding every call site.
	if onEvent == nil {
		onEvent = func(StreamEvent) {}
	}

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

	// No client-level Timeout on a streaming request.
	//
	// http.Client.Timeout covers reading the ENTIRE body, so a 120s timeout
	// severed any generation that streamed for longer than two minutes — a long
	// answer simply died mid-sentence. internal/api keeps a separate
	// timeout-free client for exactly this; mobile was still using one client
	// for both. Cancellation still works: the request carries ctx, and the
	// header phase is bounded by ResponseHeaderTimeout below.
	httpResp, err := streamHTTPClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		var errBody oaiErrorBody
		if json.Unmarshal(b, &errBody) == nil && errBody.Error.Message != "" {
			return ChatResponse{}, fmt.Errorf("API error %d: %s", httpResp.StatusCode, errBody.Error.Message)
		}
		return ChatResponse{}, fmt.Errorf("API error %d: %s", httpResp.StatusCode, string(b))
	}

	var resp ChatResponse
	// Calls in the order they started; byIndex maps a stream index to the
	// call currently using it.
	type callAccum struct {
		id, name string
		args     strings.Builder
	}
	var calls []*callAccum
	byIndex := make(map[int]*callAccum)
	sawDone := false
	lastFinish := ""

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		data, ok := sseDataPayload(scanner.Text())
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		// A failure after the 200 header (upstream overload, moderation) comes
		// as {"error":...} in the stream. It used to be skipped like any other
		// unrecognised chunk, and the caller got an empty "success".
		if chunk.Error != nil {
			msg := strings.TrimSpace(chunk.Error.Message)
			if msg == "" {
				msg = "unknown error"
			}
			if chunk.Error.Type != "" {
				msg = chunk.Error.Type + ": " + msg
			}
			return ChatResponse{}, fmt.Errorf("provider stream error: %s", msg)
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				lastFinish = *choice.FinishReason
			}
			if choice.Delta.Content != "" {
				resp.Content += choice.Delta.Content
				onEvent(StreamEvent{Delta: choice.Delta.Content})
			}
			if choice.Delta.Reasoning != "" {
				resp.ReasoningContent += choice.Delta.Reasoning
				onEvent(StreamEvent{Reasoning: choice.Delta.Reasoning})
			}
			for _, tc := range choice.Delta.ToolCalls {
				// tc.Index comes straight from the provider's JSON. A negative
				// value used to index the slice with it and panic the whole
				// mobile process; an absurd one drove unbounded growth.
				if tc.Index < 0 || tc.Index > maxStreamToolCalls {
					continue
				}
				acc, exists := byIndex[tc.Index]
				// A new id at a used index is a new call: some servers send
				// every parallel call whole at index 0, and merging by index
				// glued their argument objects into one call that did not parse.
				if !exists || (tc.ID != "" && acc.id != "" && tc.ID != acc.id) {
					if len(calls) >= maxStreamToolCalls {
						continue
					}
					acc = &callAccum{}
					byIndex[tc.Index] = acc
					calls = append(calls, acc)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return resp, err
	}
	// Neither [DONE] nor a finish_reason: the connection closed in the middle
	// of the answer, which used to be returned as a complete one.
	if !sawDone && lastFinish == "" {
		return ChatResponse{}, fmt.Errorf("stream ended before the response completed (no finish_reason or [DONE])")
	}

	for _, c := range calls {
		input := make(map[string]any)
		if raw := c.args.String(); raw != "" {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed != nil {
				input = parsed
			}
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: c.id, Name: c.name, Input: input})
	}
	return resp, nil
}

// sseDataPayload returns the payload of a server-sent-events "data" line.
//
// The space after "data:" is optional in SSE and several OpenAI-compatible
// gateways leave it out; matching only "data: " used to drop every line of
// such a stream, so the answer came back empty. A bare JSON object line is
// accepted too, for servers that stream newline-delimited JSON. (A copy of
// internal/api's helper: this package stays free of internal imports for
// gomobile bind.)
func sseDataPayload(line string) (payload string, ok bool) {
	line = strings.TrimSpace(line) // also drops the  of CRLF streams
	if strings.HasPrefix(line, "data:") {
		return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
	}
	if strings.HasPrefix(line, "{") {
		return line, true
	}
	return "", false
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
	return ChatResponse{}, fmt.Errorf("anthropic provider not yet supported on mobile")
}
