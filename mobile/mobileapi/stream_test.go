package mobileapi

import (
	"encoding/json"
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
		"stream": true,
		"max_tokens": 8192,
	}
	
	jsonBytes, _ := json.MarshalIndent(body, "", "  ")
	t.Logf("Request body:\n%s", string(jsonBytes))
}
