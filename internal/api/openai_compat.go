package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
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
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
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
	if p.apiKey == "" {
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
	Index   int    `json:"index"`
	Message oaiMsg `json:"message,omitempty"`
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
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.activeKey())

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, &RetryableError{Msg: fmt.Sprintf("http: %v", err)}
	}
	defer httpResp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10*1024*1024))
	if httpResp.StatusCode >= 500 {
		return nil, &RetryableError{Msg: fmt.Sprintf("server error %d", httpResp.StatusCode)}
	}
	if httpResp.StatusCode == 429 {
		return nil, &RetryableError{Msg: "rate limited"}
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
		for _, tc := range msg.ToolCalls {
			var input map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				continue // skip malformed tool calls
			}
			if input == nil {
				input = map[string]any{}
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
			StopReason:            "stop",
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
}

type oaiStreamChoice struct {
	Delta oaiStreamDelta `json:"delta"`
	Index int            `json:"index"`
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
	httpResp, err := retryConnectHTTP(
		streamCtx,
		defaultRetry,
		func(callCtx context.Context) (*http.Response, error) {
			httpReq, reqErr := http.NewRequestWithContext(callCtx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(data))
			if reqErr != nil {
				return nil, reqErr
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+p.activeKey())
			return sc.Do(httpReq)
		},
		func(statusCode int) bool { return statusCode >= 500 || statusCode == 429 },
	)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, formatOpenAICompatAPIError(httpResp.StatusCode, b, hadImage)
	}

	var fullContent strings.Builder
	var fullReasoningContent strings.Builder
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
	tcMap := make(map[int]*tcAccum)

	for scanner.Scan() {
		// Stop promptly if the caller cancelled (e.g. user pressed Ctrl+C).
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		markProgress() // reset the idle watchdog on every received line
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "data: [DONE]" {
			continue
		}
		clean := line
		if strings.HasPrefix(clean, "data: ") {
			clean = strings.TrimPrefix(clean, "data: ")
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(clean), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			d := chunk.Choices[0].Delta
			if d.Content != "" {
				fullContent.WriteString(d.Content)
				if handler != nil {
					handler(StreamEvent{Type: "delta", Delta: d.Content})
				}
			}
			if d.ReasoningContent != "" {
				fullReasoningContent.WriteString(d.ReasoningContent)
				if handler != nil {
					handler(StreamEvent{Type: "reasoning", Reasoning: d.ReasoningContent})
				}
			}
			for _, tc := range d.ToolCalls {
				acc, exists := tcMap[tc.Index]
				if !exists {
					acc = &tcAccum{ID: tc.ID, Name: tc.Function.Name}
					tcMap[tc.Index] = acc
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

	var toolCalls []ToolCall
	indices := make([]int, 0, len(tcMap))
	for idx := range tcMap {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		acc := tcMap[idx]
		var input map[string]any
		rawArgs := acc.ArgsBuf.String()
		if rawArgs == "" {
			continue // truncated tool call, skip
		}
		if err := json.Unmarshal([]byte(rawArgs), &input); err != nil {
			continue // incomplete JSON from truncation, skip
		}
		if input == nil {
			input = map[string]any{}
		}
		toolCalls = append(toolCalls, ToolCall{ID: acc.ID, Name: acc.Name, Input: input})
	}

	// Detect actual stop reason from last chunk's finish_reason
	stopReason := "stop"
	if len(toolCalls) == 0 && len(tcMap) > 0 {
		// Had tool calls started but none completed = truncation
		stopReason = "length"
	}

	return &ChatResponse{
		Content:               fullContent.String(),
		ReasoningContent:      fullReasoningContent.String(),
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
			return fmt.Errorf("API error %d: 当前接口不支持图片输入(image_url)。请移除附件或切换支持视觉的模型/端点。原始错误: %s", status, truncate(msg, 300))
		}
	}
	return fmt.Errorf("API error %d: %s", status, truncate(msg, 500))
}

func isReasonerModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "deepseek-reasoner") || strings.Contains(m, "deepseek-r1") || strings.Contains(m, "deepseek/deepseek-r1")
}
