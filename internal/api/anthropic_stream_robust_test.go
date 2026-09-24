package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamAnthropic serves raw as the SSE body of /v1/messages and runs
// ChatStream against it.
func streamAnthropic(t *testing.T, raw string) (*ChatResponse, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, raw)
	}))
	t.Cleanup(server.Close)
	p := &anthropicProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}
	return p.ChatStream(context.Background(), ChatRequest{
		Model:     "claude-opus-5",
		MaxTokens: 64,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	}, nil)
}

// Gateways in front of the Messages API (and DeepSeek's Anthropic endpoint)
// may write "data:" without the optional space; those events were ignored.
func TestAnthropicStreamAcceptsDataLinesWithoutSpace(t *testing.T) {
	resp, err := streamAnthropic(t, ""+
		"event: content_block_delta\n"+
		"data:{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi there\"}}\n\n"+
		"data:{\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
		"data:{\"type\":\"message_stop\"}\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if resp.Content != "hi there" {
		t.Fatalf("content = %q, want %q", resp.Content, "hi there")
	}
}

// The API reports failures after the 200 header (overloaded_error is the
// common one) as an "error" event. It was ignored, and the partial or empty
// answer came back as a successful end_turn.
func TestAnthropicStreamSurfacesErrorEvent(t *testing.T) {
	_, err := streamAnthropic(t, ""+
		"event: content_block_delta\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n"+
		"event: error\n"+
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	if err == nil {
		t.Fatal("expected the error event to be returned")
	}
	if !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("error = %q, want the API's message", err)
	}
}

// A stream that stops without message_stop or a stop_reason was cut off; it
// used to be reported as end_turn.
func TestAnthropicStreamEndingWithoutMessageStopIsAnError(t *testing.T) {
	_, err := streamAnthropic(t, ""+
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"half\"}}\n\n")
	if err == nil {
		t.Fatal("expected an error for a stream that ended before message_stop")
	}
	if !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("error = %q", err)
	}
}

// A tool call cut off by max_tokens carries incomplete arguments; running it
// would write the truncated content.
func TestAnthropicStreamDoesNotRunToolCallTruncatedByMaxTokens(t *testing.T) {
	resp, err := streamAnthropic(t, ""+
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"write\",\"input\":{}}}\n\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a.go\\\",\\\"content\\\":\\\"package ma\"}}\n\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":64}}\n\n"+
		"data: {\"type\":\"message_stop\"}\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if len(resp.ToolCalls) != 1 || !resp.ToolCalls[0].ParseError {
		t.Fatalf("tool calls = %+v, want one call flagged ParseError", resp.ToolCalls)
	}
	msg, _ := resp.ToolCalls[0].Input["_cove_parse_error"].(string)
	if !strings.Contains(msg, "output token limit") {
		t.Fatalf("diagnostic = %q, want it to explain the output limit", msg)
	}
}
