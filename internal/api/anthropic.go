package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api/adapter"
	"github.com/liuzhixin405/cove/internal/textutil"
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
	transport := defaultHTTPTransport()
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
	Source       map[string]any    `json:"source,omitempty"`
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
	// Prompt-caching counters. Anthropic reports cache activity separately from
	// input_tokens: cache_read_input_tokens are billed at a large discount and
	// cache_creation_input_tokens at a premium, and neither is included in
	// input_tokens. Ignoring them under-reports cost and lets MaxBudgetUsd drift.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// cacheHitTokens returns tokens served from the prompt cache (discounted).
func (u anthropicUsage) cacheHitTokens() int { return u.CacheReadInputTokens }

// cacheMissTokens returns non-cached prompt tokens, i.e. plain input plus the
// tokens written into the cache this request.
func (u anthropicUsage) cacheMissTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens
}

// totalInputTokens returns every prompt token the request was billed for.
func (u anthropicUsage) totalInputTokens() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// merge folds a later usage report into an earlier one. Anthropic splits usage
// across `message_start` (prompt-side counters, final) and `message_delta`
// (output_tokens, cumulative), so neither event alone is complete: taking only
// message_delta leaves InputTokens at 0 and loses all cost on the input side.
func (u *anthropicUsage) merge(next anthropicUsage) {
	if next.InputTokens > 0 {
		u.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		u.OutputTokens = next.OutputTokens
	}
	if next.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
	if next.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
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

	return retryWithBackoff(ctx, defaultRetry, func() (*ChatResponse, error) {
		return p.doChat(ctx, body)
	})
}

func (p *anthropicProvider) doChat(ctx context.Context, body anthropicReq) (*ChatResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/messages", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	key := p.activeKey()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "token-efficient-tools-2025-11-18,prompt-caching-2024-07-31")

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, &RetryableError{Msg: fmt.Sprintf("http: %v", err)}
	}
	defer httpResp.Body.Close()

	// Update the key pool's health for this key (rate-limited / dead / ok) so
	// multi-key rotation actually fails over.
	p.keyPool.MarkOutcome(key, httpResp.StatusCode, ParseRetryAfter(httpResp.Header))

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
		return nil, &StatusError{Status: httpResp.StatusCode, Msg: truncate(string(raw), 500)}
	}

	var ar anthropicResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	return &ChatResponse{
		Content:               p.extractContent(ar.Content),
		ToolCalls:             p.extractToolCalls(ar.Content),
		Model:                 ar.Model,
		InputTokens:           ar.Usage.totalInputTokens(),
		OutputTokens:          ar.Usage.OutputTokens,
		PromptCacheHitTokens:  ar.Usage.cacheHitTokens(),
		PromptCacheMissTokens: ar.Usage.cacheMissTokens(),
		StopReason:            ar.StopReason,
		RateLimitHeaders:      httpResp.Header,
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
			am.Content = convertAnthropicUserContent(m)
		}
		if len(am.Content) == 0 {
			am.Content = []anthropicContentBlock{{Type: "text", Text: ""}}
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

func convertAnthropicUserContent(m Message) []anthropicContentBlock {
	blocks := make([]anthropicContentBlock, 0, len(m.Parts)+1)
	if m.Content != "" {
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
	}
	for _, part := range m.Parts {
		switch part.Type {
		case "image":
			if part.Data == "" {
				continue
			}
			mediaType := part.MimeType
			if mediaType == "" {
				mediaType = "image/png"
			}
			blocks = append(blocks, anthropicContentBlock{
				Type: "image",
				Source: map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       part.Data,
				},
			})
		case "text", "file":
			if part.Text == "" {
				continue
			}
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: part.Text})
		}
	}
	return blocks
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

// truncate clips s to at most n bytes without splitting a rune. n is a byte
// budget here: callers use it to bound error-response bodies, which are opaque
// and may be any encoding.
func truncate(s string, n int) string {
	return textutil.ClipBytes(s, n, "...")
}

type RetryableError struct{ Msg string }

func (e *RetryableError) Error() string { return e.Msg }

// StatusError carries the HTTP status of a failed provider call.
//
// Without it, fallback.go had to classify failures by substring-matching the
// error text for "429", "500", "401" and friends — which misfires on any
// message that merely contains those digits ("max_tokens must be under 1500",
// "model gpt-4o-mini-2024-07-18"), wrongly cooling down or permanently
// blacklisting a perfectly healthy provider. Providers return this type so the
// status is read, not guessed; the substring heuristics remain only as a
// fallback for transport-level errors that carry no status at all.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string { return fmt.Sprintf("API error %d: %s", e.Status, e.Msg) }

// statusOf returns the HTTP status carried by err, or 0 when it carries none.
func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

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
	// message_start nests the initial usage snapshot under "message".
	Message *anthropicStreamMessage `json:"message,omitempty"`
}

type anthropicStreamMessage struct {
	Usage *anthropicUsage `json:"usage,omitempty"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

func (p *anthropicProvider) ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error) {
	msgs := p.convertMessages(req.Messages)
	// Mirrors Chat(): req.System is per-turn/volatile content (e.g. model-tier
	// guidance) that must NOT be folded into the cached SystemBase block, so
	// it's injected as a prepended synthetic user message instead. This was
	// previously only wired up in the non-streaming Chat() path — ChatStream
	// silently dropped req.System entirely, which matters since streaming is
	// the path actually used by the REPL.
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
		Stream:    true,
	}

	data, _ := json.Marshal(body)

	sc := p.streamClient
	if sc == nil {
		sc = p.client
	}

	// Idle watchdog: abort the stream if no data arrives for streamIdleTimeout,
	// preventing the UI from hanging forever on a silently dropped connection.
	streamCtx, markProgress, stopWatchdog := newStreamWatchdog(ctx)
	defer stopWatchdog()

	// Retry the connection-establishment phase only. Once the body starts
	// streaming, deltas have already been delivered to the handler so retrying
	// would duplicate output.
	var streamKey string
	httpResp, err := retryConnectHTTP(
		streamCtx,
		defaultRetry,
		func(callCtx context.Context) (*http.Response, error) {
			httpReq, reqErr := http.NewRequestWithContext(callCtx, "POST", p.baseURL+"/messages", bytes.NewReader(data))
			if reqErr != nil {
				return nil, reqErr
			}
			streamKey = p.activeKey()
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("x-api-key", streamKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
			httpReq.Header.Set("anthropic-beta", "token-efficient-tools-2025-11-18,prompt-caching-2024-07-31")
			return sc.Do(httpReq)
		},
		func(statusCode int) bool { return statusCode >= 500 || statusCode == 429 },
	)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	p.keyPool.MarkOutcome(streamKey, httpResp.StatusCode, ParseRetryAfter(httpResp.Header))
	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, &StatusError{Status: httpResp.StatusCode, Msg: truncate(string(body), 500)}
	}

	reader := bufio.NewReader(httpResp.Body)
	var streamAcc adapter.StreamAccumulator
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
			// Distinguish an idle-watchdog abort from a genuine read error.
			if streamCtx.Err() != nil && ctx.Err() == nil {
				return nil, fmt.Errorf("stream stalled: no data received for %s", streamIdleTimeout)
			}
			return nil, fmt.Errorf("read anthropic SSE: %w", err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		markProgress() // reset the idle watchdog on every received line
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload != "" && payload != "[DONE]" {
				var ev anthropicStreamBlock
				if err := json.Unmarshal([]byte(payload), &ev); err != nil {
					return nil, fmt.Errorf("decode anthropic SSE: %w", err)
				}

				switch ev.Type {
				case "message_start":
					// Prompt-side counters arrive here and nowhere else.
					if ev.Message != nil && ev.Message.Usage != nil {
						usage.merge(*ev.Message.Usage)
					}
					if ev.Usage != nil {
						usage.merge(*ev.Usage)
					}
				case "content_block_start":
					if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
						acc := &accumTC{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
						tcAccum[ev.Index] = acc
					}
				case "content_block_delta":
					if ev.Delta != nil {
						if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
							streamAcc.AddDelta(ev.Delta.Text)
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
						usage.merge(*ev.Usage)
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
		rawJSON := acc.JSONBuf.String()
		if rawJSON == "" {
			// Empty JSON buffer means this tool call was truncated (likely max_tokens hit)
			fmt.Fprintf(os.Stderr, "\n  [warn] tool %s: empty input (response likely truncated, stop=%s)\n", acc.Name, stopReason)
			continue
		}
		input, ok := RepairToolArguments(rawJSON)
		if !ok {
			// Incomplete/malformed JSON even after best-effort repair (tool_repair.go).
			fmt.Fprintf(os.Stderr, "\n  [warn] tool %s: failed to parse input JSON even after repair (stop=%s, raw: %s)\n", acc.Name, stopReason, truncate(rawJSON, 200))
			streamAcc.AddToolCall(adapter.ToolCall{
				ID:   acc.ID,
				Name: acc.Name,
				Input: map[string]any{"_cove_parse_error": fmt.Sprintf(
					"tool call arguments were not valid JSON and could not be auto-repaired (%d bytes, starts with: %s)",
					len(rawJSON), truncate(rawJSON, 120))},
				ParseError: true,
			})
			continue
		}
		streamAcc.AddToolCall(adapter.ToolCall{ID: acc.ID, Name: acc.Name, Input: input})
	}
	toolCalls := toAPIToolCalls(streamAcc.ToolCalls())

	if stopReason == "" {
		stopReason = "end_turn"
	}

	return &ChatResponse{
		Content:               streamAcc.Content(),
		ToolCalls:             toolCalls,
		Model:                 req.Model,
		InputTokens:           usage.totalInputTokens(),
		OutputTokens:          usage.OutputTokens,
		PromptCacheHitTokens:  usage.cacheHitTokens(),
		PromptCacheMissTokens: usage.cacheMissTokens(),
		StopReason:            stopReason,
		RateLimitHeaders:      httpResp.Header,
	}, nil
}
