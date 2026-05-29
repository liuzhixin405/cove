package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type anthropicProvider struct {
	apiKey       string
	keyPool      *KeyPool
	baseURL      string
	client       *http.Client
	streamClient *http.Client
}

func newAnthropicProvider(cfg ProviderConfig) *anthropicProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com/v1"
	}
	transport := &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 60 * time.Second,
		}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     30,
		IdleConnTimeout:     120 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	var pool *KeyPool
	if len(cfg.APIKeys) > 1 {
		pool = NewKeyPool(cfg.APIKeys)
	} else if len(cfg.APIKeys) == 1 {
		cfg.APIKey = cfg.APIKeys[0]
	}
	return &anthropicProvider{
		apiKey:  cfg.APIKey,
		keyPool: pool,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		client: &http.Client{
			Timeout:   300 * time.Second,
			Transport: transport,
		},
		streamClient: &http.Client{
			Transport: transport,
		},
	}
}

func (p *anthropicProvider) activeKey() string {
	if p.keyPool != nil {
		return p.keyPool.Get()
	}
	return p.apiKey
}

func (p *anthropicProvider) Name() string        { return "anthropic" }
func (p *anthropicProvider) DisplayName() string { return "anthropic" }
func (p *anthropicProvider) Validate() error {
	if p.apiKey == "" {
		return fmt.Errorf("API key required (set ANTHROPIC_API_KEY)")
	}
	return nil
}

type anthropicContentBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        map[string]any    `json:"input,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      any               `json:"content,omitempty"`
	IsError      *bool             `json:"is_error,omitempty"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type anthropicMsg struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicReq struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	Messages  []anthropicMsg   `json:"messages"`
	System    string           `json:"system,omitempty"`
	Tools     []map[string]any `json:"tools,omitempty"`
	Stream    bool             `json:"stream"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResp struct {
	ID         string                  `json:"id"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

func (p *anthropicProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	msgs := p.convertMessages(req.Messages)
	if req.System != "" && len(msgs) > 0 {
		msgs = append([]anthropicMsg{{Role: "user", Content: []anthropicContentBlock{
			{Type: "text", Text: req.System},
		}}}, msgs...)
	}

	body := anthropicReq{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    req.SystemBase,
		Messages:  msgs,
		Tools:     p.convertTools(req.Tools),
	}

	for attempt := 0; attempt <= defaultRetry.MaxRetries; attempt++ {
		resp, err := p.doChat(ctx, body)
		if err == nil {
			return resp, nil
		}
		if attempt == defaultRetry.MaxRetries {
			return nil, err
		}
		if isRetryable(err) {
			delay := time.Duration(math.Pow(2, float64(attempt))) * defaultRetry.BaseDelay
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("max retries exceeded")
}

func (p *anthropicProvider) doChat(ctx context.Context, body anthropicReq) (*ChatResponse, error) {
	data, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/messages", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.activeKey())
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "token-efficient-tools-2025-11-18,prompt-caching-2024-07-31")

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, &RetryableError{Msg: fmt.Sprintf("http: %v", err)}
	}
	defer httpResp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10*1024*1024))
	if httpResp.StatusCode >= 500 {
		return nil, &RetryableError{Msg: fmt.Sprintf("server error %d: %s", httpResp.StatusCode, string(raw))}
	}
	if httpResp.StatusCode == 429 {
		retryAfter := httpResp.Header.Get("Retry-After")
		delaySec := 5
		if retryAfter != "" {
			fmt.Sscanf(retryAfter, "%d", &delaySec)
		}
		return nil, &RetryableError{Msg: fmt.Sprintf("rate limited, retry after %ds", delaySec)}
	}
	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("API error %d: %s", httpResp.StatusCode, truncate(string(raw), 500))
	}

	var ar anthropicResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	return &ChatResponse{
		Content:          p.extractContent(ar.Content),
		ToolCalls:        p.extractToolCalls(ar.Content),
		Model:            ar.Model,
		InputTokens:      ar.Usage.InputTokens,
		OutputTokens:     ar.Usage.OutputTokens,
		StopReason:       ar.StopReason,
		RateLimitHeaders: httpResp.Header,
	}, nil
}

func (p *anthropicProvider) extractContent(blocks []anthropicContentBlock) string {
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func (p *anthropicProvider) extractToolCalls(blocks []anthropicContentBlock) []ToolCall {
	var calls []ToolCall
	for _, b := range blocks {
		if b.Type == "tool_use" {
			calls = append(calls, ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return calls
}

func (p *anthropicProvider) convertMessages(in []Message) []anthropicMsg {
	var out []anthropicMsg
	for _, m := range in {
		am := anthropicMsg{Role: m.Role}
		switch m.Role {
		case "tool":
			am.Content = []anthropicContentBlock{{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			}}
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					am.Content = append(am.Content, anthropicContentBlock{
						Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Input,
					})
				}
			}
			if m.Content != "" {
				am.Content = append(am.Content, anthropicContentBlock{Type: "text", Text: m.Content})
			}
		default:
			am.Content = []anthropicContentBlock{{Type: "text", Text: m.Content}}
		}
		// Inject cache_control if set on the message (prompt caching)
		if m.CacheControl != "" && len(am.Content) > 0 {
			last := &am.Content[len(am.Content)-1]
			last.CacheControl = map[string]string{"type": m.CacheControl}
		}
		out = append(out, am)
	}
	return out
}

func (p *anthropicProvider) convertTools(tools []ToolDef) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name": t.Name, "description": t.Description, "input_schema": schema,
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type RetryableError struct{ Msg string }

func (e *RetryableError) Error() string { return e.Msg }

func isRetryable(err error) bool {
	_, ok := err.(*RetryableError)
	return ok
}

type anthropicStreamBlock struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index"`
	Delta        *anthropicDelta        `json:"delta,omitempty"`
	ContentBlock *anthropicContentBlock `json:"content_block,omitempty"`
	Usage        *anthropicUsage        `json:"usage,omitempty"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

func (p *anthropicProvider) ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error) {
	msgs := p.convertMessages(req.Messages)
	body := anthropicReq{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    req.SystemBase,
		Messages:  msgs,
		Tools:     p.convertTools(req.Tools),
		Stream:    true,
	}

	data, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/messages", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.activeKey())
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "token-efficient-tools-2025-11-18,prompt-caching-2024-07-31")

	sc := p.streamClient
	if sc == nil {
		sc = p.client
	}
	httpResp, err := sc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("API error %d: %s", httpResp.StatusCode, truncate(string(body), 500))
	}

	reader := bufio.NewReader(httpResp.Body)
	var texts []string
	var toolCalls []ToolCall
	type accumTC struct {
		ID      string
		Name    string
		JSONBuf strings.Builder
	}
	tcAccum := make(map[int]*accumTC)
	var usage anthropicUsage
	var stopReason string

	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read anthropic SSE: %w", err)
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload != "" && payload != "[DONE]" {
				var ev anthropicStreamBlock
				if err := json.Unmarshal([]byte(payload), &ev); err != nil {
					return nil, fmt.Errorf("decode anthropic SSE: %w", err)
				}

				switch ev.Type {
				case "content_block_start":
					if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
						acc := &accumTC{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
						tcAccum[ev.Index] = acc
					}
				case "content_block_delta":
					if ev.Delta != nil {
						if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
							texts = append(texts, ev.Delta.Text)
							if handler != nil {
								handler(StreamEvent{Type: "delta", Delta: ev.Delta.Text})
							}
						}
						if ev.Delta.Type == "input_json_delta" && ev.Delta.PartialJSON != "" {
							if acc, ok := tcAccum[ev.Index]; ok {
								acc.JSONBuf.WriteString(ev.Delta.PartialJSON)
							}
						}
					}
				case "message_delta":
					if ev.Usage != nil {
						usage = *ev.Usage
					}
					if ev.Delta != nil && ev.Delta.StopReason != "" {
						stopReason = ev.Delta.StopReason
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}

	indices := make([]int, 0, len(tcAccum))
	for idx := range tcAccum {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		acc := tcAccum[idx]
		var input map[string]any
		rawJSON := acc.JSONBuf.String()
		if rawJSON == "" {
			// Empty JSON buffer means this tool call was truncated (likely max_tokens hit)
			fmt.Fprintf(os.Stderr, "\n  [warn] tool %s: empty input (response likely truncated, stop=%s)\n", acc.Name, stopReason)
			continue
		}
		if err := json.Unmarshal([]byte(rawJSON), &input); err != nil {
			// Incomplete JSON from truncated response - skip this tool call
			fmt.Fprintf(os.Stderr, "\n  [warn] tool %s: failed to parse input JSON: %v (stop=%s, raw: %s)\n", acc.Name, err, stopReason, truncate(rawJSON, 200))
			continue
		}
		if input == nil {
			input = map[string]any{}
		}
		toolCalls = append(toolCalls, ToolCall{ID: acc.ID, Name: acc.Name, Input: input})
	}

	if stopReason == "" {
		stopReason = "end_turn"
	}

	return &ChatResponse{
		Content:          strings.Join(texts, ""),
		ToolCalls:        toolCalls,
		Model:            req.Model,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		StopReason:       stopReason,
		RateLimitHeaders: httpResp.Header,
	}, nil
}
