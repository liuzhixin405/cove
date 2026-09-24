package api

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

	"github.com/liuzhixin405/cove/internal/api/adapter"
)

type openAICompatProvider struct {
	name         string
	apiKey       string
	keyPool      *KeyPool
	baseURL      string
	client       *http.Client // for non-streaming (has Timeout)
	streamClient *http.Client // for streaming (no global Timeout)
}

func newOpenAICompatProvider(cfg ProviderConfig) *openAICompatProvider {
	cfg.Name = NormalizeProviderName(cfg.Name)
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL(cfg.Name)
	}
	transport := defaultHTTPTransport()
	var pool *KeyPool
	if len(cfg.APIKeys) > 1 {
		pool = NewKeyPool(cfg.APIKeys)
	} else if len(cfg.APIKeys) == 1 {
		cfg.APIKey = cfg.APIKeys[0]
	}
	return &openAICompatProvider{
		name:    cfg.Name,
		apiKey:  cfg.APIKey,
		keyPool: pool,
		baseURL: normalizeOpenAIBaseURL(cfg.BaseURL),
		client: &http.Client{
			Timeout:   180 * time.Second,
			Transport: transport,
		},
		// Streaming client: no global Timeout so reading SSE body won't be killed
		// Connection-level timeouts (TLS, dial, response header) still apply
		streamClient: &http.Client{
			Transport: transport,
		},
	}
}

func (p *openAICompatProvider) activeKey() string {
	if p.keyPool != nil {
		return p.keyPool.Get()
	}
	return p.apiKey
}

func (p *openAICompatProvider) Name() string { return "openai-compatible" }
func (p *openAICompatProvider) DisplayName() string {
	if p.name == "" {
		return "openai-compatible"
	}
	return p.name
}
func (p *openAICompatProvider) Validate() error {
	if p.apiKey == "" && p.keyPool.size() == 0 {
		return fmt.Errorf("API key required (set LLM_API_KEY or provider-specific env var)")
	}
	return nil
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFuncCall `json:"function"`
}
type oaiFuncCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type oaiMsg struct {
	Role             string        `json:"role"`
	Content          any           `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}
type oaiTool struct {
	Type     string     `json:"type"`
	Function oaiFuncDef `json:"function"`
}
type oaiFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
type oaiReq struct {
	Model         string            `json:"model"`
	Messages      []oaiMsg          `json:"messages"`
	Tools         []oaiTool         `json:"tools,omitempty"`
	ToolChoice    string            `json:"tool_choice,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Stream        bool              `json:"stream,omitempty"`
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}
type oaiChoice struct {
	Index        int    `json:"index"`
	Message      oaiMsg `json:"message,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
}
type oaiUsage struct {
	PromptTokens            int                        `json:"prompt_tokens"`
	CompletionTokens        int                        `json:"completion_tokens"`
	PromptCacheHitTokens    int                        `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens   int                        `json:"prompt_cache_miss_tokens,omitempty"`
	PromptTokensDetails     *oaiPromptTokensDetails    `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *oaiCompletionTokenDetails `json:"completion_tokens_details,omitempty"`
}

type oaiPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type oaiCompletionTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}
type oaiResp struct {
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage,omitempty"`
}

func (p *openAICompatProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	normalizedMessages := downgradeImagePartsForNonVision(req.Messages, req.Model)
	msgs := p.convertMessages(normalizedMessages)
	if req.System != "" || req.SystemBase != "" {
		combined := req.SystemBase
		if combined != "" && req.System != "" {
			combined += "\n\n"
		}
		combined += req.System
		msgs = append([]oaiMsg{{Role: "system", Content: combined}}, msgs...)
	}

	var tools []oaiTool
	var toolChoice string
	if !isReasonerModel(req.Model) {
		tools = p.convertTools(req.Tools)
		toolChoice = p.toolChoice(tools)
	}

	body := oaiReq{
		Model:      req.Model,
		Messages:   msgs,
		Tools:      tools,
		ToolChoice: toolChoice,
		MaxTokens:  req.MaxTokens,
	}

	return retryWithBackoff(ctx, defaultRetry, func() (*ChatResponse, error) {
		return p.doChat(ctx, body)
	})
}

func (p *openAICompatProvider) doChat(ctx context.Context, body oaiReq) (*ChatResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	hadImage := oaiReqHasImageURL(body)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	key := p.activeKey()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key)

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, &RetryableError{Msg: fmt.Sprintf("http: %v", err)}
	}
	defer func() { _ = httpResp.Body.Close() }()

	// Update key-pool health so multi-key rotation fails over.
	p.keyPool.MarkOutcome(key, httpResp.StatusCode, ParseRetryAfter(httpResp.Header))

	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10*1024*1024))
	if httpResp.StatusCode >= 500 || httpResp.StatusCode == http.StatusTooManyRequests {
		return nil, &RetryableError{Msg: truncate(string(raw), 500), Status: httpResp.StatusCode, RetryAfter: ParseRetryAfter(httpResp.Header)}
	}
	if httpResp.StatusCode != 200 {
		return nil, formatOpenAICompatAPIError(httpResp.StatusCode, raw, hadImage)
	}

	var cr oaiResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	var content string
	var toolCalls []ToolCall
	if len(cr.Choices) > 0 {
		msg := cr.Choices[0].Message
		content = extractOAIMsgText(msg.Content)
		reasoningContent := msg.ReasoningContent
		if cr.Choices[0].FinishReason == finishInsufficientResource {
			return nil, errInsufficientResource()
		}
		stopReason := openAIStopReason(cr.Choices[0].FinishReason)
		for _, tc := range msg.ToolCalls {
			if tc.ID == "" {
				tc.ID = newToolCallID()
			}
			input, ok := RepairToolArguments(tc.Function.Arguments)
			if !ok {
				toolCalls = append(toolCalls, ToolCall{
					ID:         tc.ID,
					Name:       tc.Function.Name,
					Input:      toolArgsParseError(tc.Function.Arguments, stopReason == "length"),
					ParseError: true,
				})
				continue
			}
			toolCalls = append(toolCalls, ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Input: input,
			})
		}
		return &ChatResponse{
			Content:               content,
			ReasoningContent:      reasoningContent,
			ToolCalls:             toolCalls,
			Model:                 cr.Model,
			InputTokens:           cr.Usage.PromptTokens,
			OutputTokens:          cr.Usage.CompletionTokens,
			PromptCacheHitTokens:  cr.Usage.cacheHitTokens(),
			PromptCacheMissTokens: cr.Usage.cacheMissTokens(),
			ReasoningTokens:       cr.Usage.reasoningTokens(),
			StopReason:            stopReason,
			RateLimitHeaders:      httpResp.Header,
		}, nil
	}

	return &ChatResponse{
		Content:               content,
		ToolCalls:             toolCalls,
		Model:                 cr.Model,
		InputTokens:           cr.Usage.PromptTokens,
		OutputTokens:          cr.Usage.CompletionTokens,
		PromptCacheHitTokens:  cr.Usage.cacheHitTokens(),
		PromptCacheMissTokens: cr.Usage.cacheMissTokens(),
		ReasoningTokens:       cr.Usage.reasoningTokens(),
		StopReason:            "stop",
		RateLimitHeaders:      httpResp.Header,
	}, nil
}

func (u oaiUsage) cacheHitTokens() int {
	if u.PromptCacheHitTokens > 0 {
		return u.PromptCacheHitTokens
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

func (u oaiUsage) cacheMissTokens() int {
	if u.PromptCacheMissTokens > 0 {
		return u.PromptCacheMissTokens
	}
	hit := u.cacheHitTokens()
	if u.PromptTokens > hit {
		return u.PromptTokens - hit
	}
	return 0
}

func (u oaiUsage) reasoningTokens() int {
	if u.CompletionTokensDetails != nil {
		return u.CompletionTokensDetails.ReasoningTokens
	}
	return 0
}

func (p *openAICompatProvider) convertMessages(in []Message) []oaiMsg {
	var out []oaiMsg
	for _, m := range in {
		// Consecutive plain-text user turns (a real message followed by
		// engine-supplied context) go out as one message: several compatible
		// backends reject successive user messages.
		if n := len(out); n > 0 && m.Role == "user" && len(m.Parts) == 0 && out[n-1].Role == "user" {
			if prev, ok := out[n-1].Content.(string); ok {
				out[n-1].Content = prev + "\n\n" + m.Content
				continue
			}
		}
		om := oaiMsg{Role: m.Role, Content: p.convertMessageContent(m), ToolCallID: m.ToolCallID}
		if len(m.ToolCalls) > 0 {
			// DeepSeek Think/Tool-use guidelines:
			// 1. If the assistant performed tool calls, its reasoning_content MUST be included in the
			//    context history and sent back to the API in subsequent turns.
			// 2. If NO tool calls were made, reasoning_content can be safely omitted/stripped to
			//    avoid bloat or errors since it is ignored by the API anyway.
			om.ReasoningContent = m.ReasoningContent

			for _, tc := range m.ToolCalls {
				args, _ := json.Marshal(tc.Input)
				om.ToolCalls = append(om.ToolCalls, oaiToolCall{
					ID: tc.ID, Type: "function",
					Function: oaiFuncCall{Name: tc.Name, Arguments: string(args)},
				})
			}
		}
		out = append(out, om)
	}
	return out
}

func (p *openAICompatProvider) convertMessageContent(m Message) any {
	if len(m.Parts) == 0 {
		return m.Content
	}
	blocks := make([]map[string]any, 0, len(m.Parts)+1)
	if m.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
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
			blocks = append(blocks, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + mediaType + ";base64," + part.Data},
			})
		case "text", "file":
			if part.Text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
		}
	}
	if len(blocks) == 0 {
		return m.Content
	}
	return blocks
}

func extractOAIMsgText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ != "text" {
				continue
			}
			text, _ := m["text"].(string)
			if text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(text)
		}
		return sb.String()
	default:
		return ""
	}
}

func (p *openAICompatProvider) convertTools(tools []ToolDef) []oaiTool {
	var out []oaiTool
	for _, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, oaiTool{
			Type:     "function",
			Function: oaiFuncDef{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	return out
}

func (p *openAICompatProvider) toolChoice(tools []oaiTool) string {
	if len(tools) == 0 {
		return ""
	}
	return "auto"
}

type oaiStreamChunk struct {
	Choices []oaiStreamChoice `json:"choices"`
	Usage   *oaiUsage         `json:"usage,omitempty"`
	// Error is how compatible servers report a failure that happens after the
	// 200 header was sent (upstream overload, moderation, a dropped backend).
	Error *oaiStreamError `json:"error,omitempty"`
}

type oaiStreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type oaiStreamChoice struct {
	Delta        oaiStreamDelta `json:"delta"`
	Index        int            `json:"index"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

type oaiStreamDelta struct {
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiStreamTC `json:"tool_calls,omitempty"`
}

type oaiStreamTC struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Function oaiFuncCall `json:"function,omitempty"`
}

func (p *openAICompatProvider) ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error) {
	normalizedMessages := downgradeImagePartsForNonVision(req.Messages, req.Model)
	messages := []oaiMsg{}
	if req.SystemBase != "" || req.System != "" {
		combined := req.SystemBase
		if combined != "" && req.System != "" {
			combined += "\n\n"
		}
		combined += req.System
		messages = append(messages, oaiMsg{Role: "system", Content: combined})
	}
	messages = append(messages, p.convertMessages(normalizedMessages)...)

	var tools []oaiTool
	var toolChoice string
	if !isReasonerModel(req.Model) {
		tools = p.convertTools(req.Tools)
		toolChoice = p.toolChoice(tools)
	}

	body := oaiReq{
		Model:         req.Model,
		Messages:      messages,
		Tools:         tools,
		ToolChoice:    toolChoice,
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
	}
	hadImage := oaiReqHasImageURL(body)

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
	// would duplicate output; we therefore never retry after streaming begins.
	var streamKey string
	httpResp, err := retryConnectHTTP(
		streamCtx,
		defaultRetry,
		func(callCtx context.Context) (*http.Response, error) {
			httpReq, reqErr := http.NewRequestWithContext(callCtx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(data))
			if reqErr != nil {
				return nil, reqErr
			}
			streamKey = p.activeKey()
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+streamKey)
			return sc.Do(httpReq)
		},
		func(statusCode int) bool { return statusCode >= 500 || statusCode == 429 },
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	p.keyPool.MarkOutcome(streamKey, httpResp.StatusCode, ParseRetryAfter(httpResp.Header))

	if httpResp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, formatOpenAICompatAPIError(httpResp.StatusCode, b, hadImage)
	}

	var streamAcc adapter.StreamAccumulator
	var usage oaiUsage
	scanner := bufio.NewScanner(httpResp.Body)
	// Default scanner buffer is 64KB — insufficient for large tool call arguments
	// DeepSeek may send entire file content in a single SSE line
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // up to 10MB per line

	type tcAccum struct {
		ID      string
		Name    string
		ArgsBuf strings.Builder
	}
	// Calls in the order they started; byIndex maps a stream index to the
	// call currently using it.
	var calls []*tcAccum
	byIndex := make(map[int]*tcAccum)
	lastFinish := "" // finish_reason from the most recent chunk that reported one
	sawDone := false

	for scanner.Scan() {
		// Stop promptly if the caller cancelled (e.g. user pressed Ctrl+C).
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		markProgress() // reset the idle watchdog on every received line
		payload, ok := sseDataPayload(scanner.Text())
		if !ok {
			continue // blank line, ": keep-alive" comment, event:/id: field
		}
		if payload == "[DONE]" {
			sawDone = true
			break
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("provider stream error: %s", streamErrorText(chunk.Error))
		}
		if len(chunk.Choices) > 0 {
			if fr := chunk.Choices[0].FinishReason; fr != "" {
				lastFinish = fr
			}
			d := chunk.Choices[0].Delta
			if d.Content != "" {
				streamAcc.AddDelta(d.Content)
				if handler != nil {
					handler(StreamEvent{Type: "delta", Delta: d.Content})
				}
			}
			if d.ReasoningContent != "" {
				streamAcc.AddReasoning(d.ReasoningContent)
				if handler != nil {
					handler(StreamEvent{Type: "reasoning", Reasoning: d.ReasoningContent})
				}
			}
			for _, tc := range d.ToolCalls {
				acc, exists := byIndex[tc.Index]
				// A new id at a used index is a new call: some servers send
				// every parallel call whole at index 0, and merging them by
				// index glued two argument objects into one broken call.
				if !exists || (tc.ID != "" && acc.ID != "" && tc.ID != acc.ID) {
					acc = &tcAccum{ID: tc.ID, Name: tc.Function.Name}
					byIndex[tc.Index] = acc
					calls = append(calls, acc)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.ArgsBuf.WriteString(tc.Function.Arguments)
			}
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
	}

	// Check for scanner errors (e.g. line too long even with expanded buffer)
	if err := scanner.Err(); err != nil {
		// If the watchdog cancelled the read (idle stall) while the caller did
		// not cancel, surface a clear timeout error instead of a generic one.
		if streamCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("stream stalled: no data received for %s", streamIdleTimeout)
		}
		return nil, fmt.Errorf("stream read error: %w", err)
	}
	// Neither a finish_reason nor [DONE]: the connection closed cleanly in
	// the middle of the answer. This used to be returned as a normal "stop",
	// so a half-written reply was taken as the final one.
	if !sawDone && lastFinish == "" {
		return nil, fmt.Errorf("stream ended before the response completed (unexpected EOF: no finish_reason or [DONE])")
	}

	if lastFinish == finishInsufficientResource {
		return nil, errInsufficientResource()
	}

	truncated := lastFinish == "length"
	for _, acc := range calls {
		rawArgs := acc.ArgsBuf.String()
		if acc.ID == "" {
			acc.ID = newToolCallID()
		}
		if strings.TrimSpace(rawArgs) == "" {
			if truncated || acc.Name == "" {
				continue // cut off before any arguments arrived
			}
			// A tool without parameters may stream no argument text at all;
			// that is a complete call with an empty input.
			streamAcc.AddToolCall(adapter.ToolCall{ID: acc.ID, Name: acc.Name, Input: map[string]any{}})
			continue
		}
		input, ok := RepairToolArguments(rawArgs)
		if !ok {
			streamAcc.AddToolCall(adapter.ToolCall{
				ID:         acc.ID,
				Name:       acc.Name,
				Input:      toolArgsParseError(rawArgs, truncated),
				ParseError: true,
			})
			continue
		}
		streamAcc.AddToolCall(adapter.ToolCall{ID: acc.ID, Name: acc.Name, Input: input})
	}
	toolCalls := toAPIToolCalls(streamAcc.ToolCalls())

	stopReason := openAIStopReason(lastFinish)
	if stopReason == "stop" && len(toolCalls) == 0 && len(calls) > 0 {
		// Tool calls started streaming but none completed → truncated mid-call.
		stopReason = "length"
	}

	return &ChatResponse{
		Content:               streamAcc.Content(),
		ReasoningContent:      streamAcc.Reasoning(),
		ToolCalls:             toolCalls,
		Model:                 req.Model,
		InputTokens:           usage.PromptTokens,
		OutputTokens:          usage.CompletionTokens,
		PromptCacheHitTokens:  usage.cacheHitTokens(),
		PromptCacheMissTokens: usage.cacheMissTokens(),
		ReasoningTokens:       usage.reasoningTokens(),
		StopReason:            stopReason,
		RateLimitHeaders:      httpResp.Header,
	}, nil
}

// finishInsufficientResource is DeepSeek's finish_reason for inference it
// aborted under load. The answer is incomplete; it used to be accepted as a
// finished turn.
const finishInsufficientResource = "insufficient_system_resource"

func errInsufficientResource() error {
	return &RetryableError{Status: http.StatusServiceUnavailable,
		Msg: "the server aborted inference (finish_reason insufficient_system_resource); the answer is incomplete"}
}

// openAIStopReason maps an OpenAI finish_reason ("stop" | "length" |
// "tool_calls" | "content_filter") to the vocabulary the engine checks: it
// treats "length" with no tool calls as a truncation that needs continuation.
func openAIStopReason(finish string) string {
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use"
	case "", "stop":
		return "stop"
	default:
		return finish
	}
}

func toAPIToolCalls(calls []adapter.ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{
			ID:         c.ID,
			Name:       c.Name,
			Input:      c.Input,
			ParseError: c.ParseError,
		})
	}
	return out
}

func downgradeImagePartsForNonVision(messages []Message, model string) []Message {
	if IsVisionCapableModel(model) {
		return messages
	}
	out := make([]Message, len(messages))
	for i, m := range messages {
		cp := m
		if len(m.Parts) == 0 {
			out[i] = cp
			continue
		}
		cp.Parts = make([]MessagePart, 0, len(m.Parts))
		for _, part := range m.Parts {
			if part.Type != "image" {
				cp.Parts = append(cp.Parts, part)
				continue
			}
			name := part.FileName
			if name == "" {
				name = "image"
			}
			cp.Parts = append(cp.Parts, MessagePart{
				Type: "text",
				Text: fmt.Sprintf("[图片附件 %s 已自动降级：当前模型 %s 可能不支持视觉输入。请切换视觉模型后重试。]", name, model),
			})
		}
		out[i] = cp
	}
	return out
}

func oaiReqHasImageURL(req oaiReq) bool {
	for _, m := range req.Messages {
		arr, ok := m.Content.([]map[string]any)
		if !ok {
			continue
		}
		for _, block := range arr {
			if t, _ := block["type"].(string); t == "image_url" {
				return true
			}
		}
	}
	return false
}

func formatOpenAICompatAPIError(status int, raw []byte, hadImage bool) error {
	msg := string(raw)
	if hadImage {
		lower := strings.ToLower(msg)
		if strings.Contains(msg, "unknown variant `image_url`") ||
			(strings.Contains(lower, "image_url") && strings.Contains(lower, "expected `text`")) {
			return &StatusError{Status: status, Msg: fmt.Sprintf("当前接口不支持图片输入(image_url)。请移除附件或切换支持视觉的模型/端点。原始错误: %s", truncate(msg, 300))}
		}
	}
	return &StatusError{Status: status, Msg: truncate(msg, 500)}
}

func isReasonerModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "deepseek-reasoner") || strings.Contains(m, "deepseek-r1") || strings.Contains(m, "deepseek/deepseek-r1")
}
