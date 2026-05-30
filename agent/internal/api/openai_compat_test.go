package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatChatCarriesReasoningContentIntoResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"deepseek-v4-pro",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"",
					"reasoning_content":"need tool first",
					"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}
					}]
				}
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":3}
		}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{
		apiKey:  "test-key",
		baseURL: server.URL + "/v1",
		client:  server.Client(),
	}

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:     "deepseek-v4-pro",
		MaxTokens: 128,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if got, want := resp.ReasoningContent, "need tool first"; got != want {
		t.Fatalf("ReasoningContent = %q, want %q", got, want)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
}

func TestOpenAICompatConvertMessagesIncludesReasoningContent(t *testing.T) {
	p := &openAICompatProvider{}
	msgs := p.convertMessages([]Message{{
		Role:             "assistant",
		Content:          "",
		ReasoningContent: "internal reasoning",
		ToolCalls: []ToolCall{{
			ID:    "call_1",
			Name:  "read_file",
			Input: map[string]any{"path": "README.md"},
		}},
	}, {
		Role:       "tool",
		ToolCallID: "call_1",
		Content:    "# agentgo",
	}})

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if got, want := msgs[0].ReasoningContent, "internal reasoning"; got != want {
		t.Fatalf("assistant reasoning_content = %q, want %q", got, want)
	}
	if got := msgs[1].ToolCallID; got != "call_1" {
		t.Fatalf("tool message tool_call_id = %q, want call_1", got)
	}
}

func TestOpenAICompatChatParsesDeepSeekCacheUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"deepseek-v4-pro",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],
			"usage":{
				"prompt_tokens":100,
				"completion_tokens":20,
				"prompt_cache_hit_tokens":60,
				"prompt_cache_miss_tokens":40,
				"completion_tokens_details":{"reasoning_tokens":7}
			}
		}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL, client: server.Client()}
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-pro", MaxTokens: 128, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.PromptCacheHitTokens != 60 || resp.PromptCacheMissTokens != 40 {
		t.Fatalf("unexpected cache usage: hit=%d miss=%d", resp.PromptCacheHitTokens, resp.PromptCacheMissTokens)
	}
	if resp.ReasoningTokens != 7 {
		t.Fatalf("ReasoningTokens = %d, want 7", resp.ReasoningTokens)
	}
}

func TestOpenAICompatChatStreamCapturesReasoningContentAndToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"need tool\"}}],\"usage\":null}\n\n",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\"}}]}}],\"usage\":null}\n\n",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"path\\\":\\\"README.md\\\"}\"}}]}}],\"usage\":null}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":5,\"prompt_cache_miss_tokens\":6,\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, chunk := range chunks {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	p := &openAICompatProvider{
		apiKey:  "test-key",
		baseURL: server.URL + "/v1",
		client:  server.Client(),
	}

	var deltas []string
	resp, err := p.ChatStream(context.Background(), ChatRequest{
		Model:     "deepseek-v4-pro",
		MaxTokens: 128,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	}, func(ev StreamEvent) {
		if ev.Type == "delta" {
			deltas = append(deltas, ev.Delta)
		}
	})
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	if got, want := resp.ReasoningContent, "need tool"; got != want {
		t.Fatalf("ReasoningContent = %q, want %q", got, want)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if got, want := resp.ToolCalls[0].Input["path"], any("README.md"); got != want {
		t.Fatalf("tool input path = %#v, want %#v", got, want)
	}
	if got, want := resp.InputTokens, 11; got != want {
		t.Fatalf("InputTokens = %d, want %d", got, want)
	}
	if got, want := resp.OutputTokens, 7; got != want {
		t.Fatalf("OutputTokens = %d, want %d", got, want)
	}
	if got, want := resp.PromptCacheHitTokens, 5; got != want {
		t.Fatalf("PromptCacheHitTokens = %d, want %d", got, want)
	}
	if got, want := resp.PromptCacheMissTokens, 6; got != want {
		t.Fatalf("PromptCacheMissTokens = %d, want %d", got, want)
	}
	if got, want := resp.ReasoningTokens, 3; got != want {
		t.Fatalf("ReasoningTokens = %d, want %d", got, want)
	}
	if got := strings.Join(deltas, ""); got != "" {
		t.Fatalf("expected no content deltas, got %q", got)
	}
}

func TestOpenAICompatChatStreamRequestsUsageInStreamOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		streamOptions, ok := body["stream_options"].(map[string]any)
		if !ok {
			t.Fatalf("stream_options missing: %#v", body)
		}
		if got, ok := streamOptions["include_usage"].(bool); !ok || !got {
			t.Fatalf("include_usage = %#v, want true", streamOptions["include_usage"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL + "/v1", client: server.Client()}
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "deepseek-v4-pro", MaxTokens: 32, Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	if resp.InputTokens != 1 || resp.OutputTokens != 1 {
		t.Fatalf("unexpected usage: in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
}

func TestOpenAICompatConvertMessagesSupportsImageParts(t *testing.T) {
	p := &openAICompatProvider{}
	msgs := p.convertMessages([]Message{{
		Role: "user",
		Parts: []MessagePart{
			{Type: "text", Text: "请看这张图"},
			{Type: "image", MimeType: "image/png", Data: "aGVsbG8="},
		},
	}})

	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	arr, ok := msgs[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("expected content array, got %T", msgs[0].Content)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(arr))
	}
	if arr[0]["type"] != "text" {
		t.Fatalf("first block type = %#v, want text", arr[0]["type"])
	}
	if arr[1]["type"] != "image_url" {
		t.Fatalf("second block type = %#v, want image_url", arr[1]["type"])
	}
}
