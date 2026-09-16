package mobileapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseStreamChunk(t *testing.T) {
	// Simulate a typical DeepSeek streaming response with content
	data := `{"choices":[{"delta":{"content":"Hello","role":"assistant"},"finish_reason":null}]}`

	var chunk oaiStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(chunk.Choices) == 0 {
		t.Fatal("no choices")
	}

	choice := chunk.Choices[0]
	if choice.Delta.Content != "Hello" {
		t.Fatalf("expected content 'Hello', got '%s'", choice.Delta.Content)
	}

	t.Logf("Content: %s", choice.Delta.Content)
	t.Logf("FinishReason: %v", choice.FinishReason)
}

func TestParseStreamChunkWithToolCalls(t *testing.T) {
	// Simulate a DeepSeek streaming response with tool calls
	data := `{"choices":[{"delta":{"content":null,"role":"assistant","tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"screenshot","arguments":""}}]},"finish_reason":null}]}`

	var chunk oaiStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(chunk.Choices) == 0 {
		t.Fatal("no choices")
	}

	choice := chunk.Choices[0]
	t.Logf("Content: '%s'", choice.Delta.Content)
	t.Logf("ToolCalls count: %d", len(choice.Delta.ToolCalls))

	if len(choice.Delta.ToolCalls) > 0 {
		tc := choice.Delta.ToolCalls[0]
		t.Logf("ToolCall ID: %s, Name: %s, Args: %s", tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
}

func TestParseNonStreamResponse(t *testing.T) {
	// What if the API returns a non-streaming response instead?
	data := `{"id":"chatcmpl-123","object":"chat.completion","created":1677652288,"model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"Hello! How can I help you?"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":9,"total_tokens":18}}`

	var chunk oaiStreamChunk
	err := json.Unmarshal([]byte(data), &chunk)
	if err != nil {
		t.Logf("Non-streaming JSON cannot be parsed as streaming chunk (expected): %v", err)
	} else {
		t.Logf("Parsed as streaming chunk: choices=%d", len(chunk.Choices))
	}
}

func TestBuildChatRequestBody(t *testing.T) {
	// Test the full request body construction
	body := map[string]interface{}{
		"model": "deepseek-chat",
		"messages": []map[string]interface{}{
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello"},
		},
		"stream":     true,
		"max_tokens": 8192,
	}

	jsonBytes, _ := json.MarshalIndent(body, "", "  ")
	t.Logf("Request body:\n%s", string(jsonBytes))
}

// TestChatStreamNegativeToolIndex is the regression test for a crash on
// malformed SSE: tc.Index came straight from provider JSON, and a negative
// value fell through to resp.ToolCalls[-1], panicking the mobile process.
func TestChatStreamNegativeToolIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":-1,"id":"x","function":{"name":"bad","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":-99,"function":{"arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			`data: [DONE]`,
		} {
			io.WriteString(w, chunk+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	p := newOpenAICompatProvider(ProviderConfig{Name: "openai", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "gpt-4o"}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want %q", resp.Content, "ok")
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("got %d tool calls from negative indices, want 0", len(resp.ToolCalls))
	}
}

// TestChatStreamAbsurdToolIndex guards the upper bound: a huge index must not
// drive an unbounded slice growth.
func TestChatStreamAbsurdToolIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":100000000,"id":"x","function":{"name":"n","arguments":"{}"}}]}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newOpenAICompatProvider(ProviderConfig{Name: "openai", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "gpt-4o"}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("got %d tool calls from an absurd index, want 0", len(resp.ToolCalls))
	}
}

// TestChatStreamNilCallback pins the nil-onEvent contract for the mobile
// binding: a caller that only wants the final response must not panic.
func TestChatStreamNilCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newOpenAICompatProvider(ProviderConfig{Name: "openai", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "gpt-4o"}, nil)
	if err != nil {
		t.Fatalf("ChatStream with nil callback: %v", err)
	}
	if resp.Content != "hi" {
		t.Fatalf("Content = %q, want %q", resp.Content, "hi")
	}
}
