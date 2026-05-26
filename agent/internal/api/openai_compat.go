package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type openAICompatProvider struct {
	name    string
	apiKey  string
	baseURL string
	client  *http.Client
}

func newOpenAICompatProvider(cfg ProviderConfig) *openAICompatProvider {
	cfg.Name = NormalizeProviderName(cfg.Name)
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL(cfg.Name)
	}
	return &openAICompatProvider{
		name:    cfg.Name,
		apiKey:  cfg.APIKey,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		client:  &http.Client{Timeout: 180 * time.Second},
	}
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
	Content          string        `json:"content"`
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
	msgs := p.convertMessages(req.Messages)
	if req.System != "" || req.SystemBase != "" {
		combined := req.SystemBase
		if combined != "" && req.System != "" {
			combined += "\n\n"
		}
		combined += req.System
		msgs = append([]oaiMsg{{Role: "system", Content: combined}}, msgs...)
	}

	tools := p.convertTools(req.Tools)
	body := oaiReq{
		Model:      req.Model,
		Messages:   msgs,
		Tools:      tools,
		ToolChoice: p.toolChoice(tools),
		MaxTokens:  req.MaxTokens,
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

func (p *openAICompatProvider) doChat(ctx context.Context, body oaiReq) (*ChatResponse, error) {
	data, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

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
		return nil, fmt.Errorf("API error %d: %s", httpResp.StatusCode, truncate(string(raw), 500))
	}

	var cr oaiResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	var content string
	var toolCalls []ToolCall
	if len(cr.Choices) > 0 {
		msg := cr.Choices[0].Message
		content = msg.Content
		reasoningContent := msg.ReasoningContent
		for _, tc := range msg.ToolCalls {
			var input map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &input)
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
		om := oaiMsg{Role: m.Role, Content: m.Content, ReasoningContent: m.ReasoningContent, ToolCallID: m.ToolCallID}
		if len(m.ToolCalls) > 0 {
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
	messages := []oaiMsg{}
	if req.SystemBase != "" || req.System != "" {
		combined := req.SystemBase
		if combined != "" && req.System != "" {
			combined += "\n\n"
		}
		combined += req.System
		messages = append(messages, oaiMsg{Role: "system", Content: combined})
	}
	messages = append(messages, p.convertMessages(req.Messages)...)

	tools := p.convertTools(req.Tools)
	body := oaiReq{
		Model:         req.Model,
		Messages:      messages,
		Tools:         tools,
		ToolChoice:    p.toolChoice(tools),
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
	}

	data, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("stream API error %d: %s", httpResp.StatusCode, string(b))
	}

	var fullContent strings.Builder
	var fullReasoningContent strings.Builder
	var usage oaiUsage
	scanner := bufio.NewScanner(httpResp.Body)

	type tcAccum struct {
		ID      string
		Name    string
		ArgsBuf strings.Builder
	}
	tcMap := make(map[int]*tcAccum)

	for scanner.Scan() {
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

	var toolCalls []ToolCall
	indices := make([]int, 0, len(tcMap))
	for idx := range tcMap {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		acc := tcMap[idx]
		var input map[string]any
		json.Unmarshal([]byte(acc.ArgsBuf.String()), &input)
		if input == nil {
			input = map[string]any{}
		}
		toolCalls = append(toolCalls, ToolCall{ID: acc.ID, Name: acc.Name, Input: input})
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
		StopReason:            "stop",
	}, nil
}
