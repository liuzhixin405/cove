package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamOpenAI serves raw as the SSE body of /v1/chat/completions and runs
// ChatStream against it, returning the response, the text deltas the handler
// saw and the error.
func streamOpenAI(t *testing.T, raw string) (*ChatResponse, string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, raw)
	}))
	t.Cleanup(server.Close)

	p := &openAICompatProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}
	var deltas strings.Builder
	resp, err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(ev StreamEvent) {
		if ev.Type == "delta" {
			deltas.WriteString(ev.Delta)
		}
	})
	return resp, deltas.String(), err
}

// The SSE spec makes the space after "data:" optional, and several
// OpenAI-compatible gateways omit it. Those lines used to fail to decode and
// were skipped silently, so the whole answer came back empty.
func TestOpenAIStreamAcceptsDataLinesWithoutSpace(t *testing.T) {
	resp, deltas, err := streamOpenAI(t, ""+
		"data:{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"}}]}\n\n"+
		"data:{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"+
		"data:[DONE]\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if resp.Content != "hello" || deltas != "hello" {
		t.Fatalf("content = %q, deltas = %q, want hello", resp.Content, deltas)
	}
}

// CRLF line endings, keep-alive comments and event: lines are all legal SSE
// and must not disturb parsing.
func TestOpenAIStreamToleratesCRLFAndKeepAliveComments(t *testing.T) {
	resp, _, err := streamOpenAI(t, ""+
		": keep-alive\r\n\r\n"+
		"event: message\r\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\r\n\r\n"+
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\r\n\r\n"+
		"data: [DONE]\r\n\r\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if resp.Content != "ok" || resp.InputTokens != 3 || resp.StopReason != "stop" {
		t.Fatalf("resp = %+v", resp)
	}
}

// A stream that ends without finish_reason and without [DONE] was cut off
// (proxy restart, dropped connection that closed cleanly). It used to come
// back as a normal "stop" answer, so a half-written reply was taken as final.
func TestOpenAIStreamEndingWithoutCompletionIsAnError(t *testing.T) {
	_, deltas, err := streamOpenAI(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial ans\"}}]}\n\n")
	if err == nil {
		t.Fatal("expected an error for a stream that ended before completing")
	}
	if deltas != "partial ans" {
		t.Fatalf("deltas = %q", deltas)
	}
	if !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("error = %q, want it to say the stream ended early", err)
	}
}

// A finish_reason without [DONE] is still a complete answer: some servers
// never send the [DONE] sentinel.
func TestOpenAIStreamFinishReasonWithoutDoneIsComplete(t *testing.T) {
	resp, _, err := streamOpenAI(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if resp.Content != "x" {
		t.Fatalf("content = %q", resp.Content)
	}
}

// Providers report failures that happen after the 200 header as an error
// object inside the stream. Ignoring it returned an empty "successful" answer.
func TestOpenAIStreamSurfacesInStreamErrorObject(t *testing.T) {
	_, _, err := streamOpenAI(t, ""+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"+
		"data: {\"error\":{\"message\":\"upstream overloaded\",\"type\":\"server_error\"}}\n\n"+
		"data: [DONE]\n\n")
	if err == nil {
		t.Fatal("expected the in-stream error to be returned")
	}
	if !strings.Contains(err.Error(), "upstream overloaded") {
		t.Fatalf("error = %q, want the provider's message", err)
	}
}

// A tool that takes no parameters can stream no argument fragments at all.
// That call used to be dropped, and the empty-but-started call then flipped
// the stop reason to "length", sending the engine into a bogus
// "your response was truncated" continuation.
func TestOpenAIStreamKeepsToolCallWithEmptyArguments(t *testing.T) {
	resp, _, err := streamOpenAI(t, ""+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"todoread\",\"arguments\":\"\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
		"data: [DONE]\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "todoread" || resp.ToolCalls[0].ParseError {
		t.Fatalf("tool calls = %+v, want one todoread call", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Input == nil || len(resp.ToolCalls[0].Input) != 0 {
		t.Fatalf("input = %#v, want empty object", resp.ToolCalls[0].Input)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", resp.StopReason)
	}
}

// Parallel tool calls stream interleaved by index; each keeps its own id and
// arguments.
func TestOpenAIStreamAssemblesInterleavedParallelToolCalls(t *testing.T) {
	resp, _, err := streamOpenAI(t, ""+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"\\\"b.go\\\"}\"}},{\"index\":0,\"function\":{\"arguments\":\"\\\"a.go\\\"}\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
		"data: [DONE]\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].ID != "a" || resp.ToolCalls[0].Input["path"] != "a.go" ||
		resp.ToolCalls[1].ID != "b" || resp.ToolCalls[1].Input["path"] != "b.go" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}

// Some compatible servers send every parallel call whole, each at index 0
// with its own id. Merging them by index concatenated two argument objects
// into one unparseable call.
func TestOpenAIStreamSeparatesCallsThatReuseIndexWithNewID(t *testing.T) {
	resp, _, err := streamOpenAI(t, ""+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"b\",\"function\":{\"name\":\"glob\",\"arguments\":\"{\\\"pattern\\\":\\\"*.go\\\"}\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
		"data: [DONE]\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2 separate calls", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Name != "read" || resp.ToolCalls[0].Input["path"] != "a.go" ||
		resp.ToolCalls[1].Name != "glob" || resp.ToolCalls[1].Input["pattern"] != "*.go" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}

// A tool call without an id cannot be answered: the tool result's
// tool_call_id would be empty and the next request is rejected. Give it one.
func TestOpenAIStreamAssignsIDToToolCallWithoutOne(t *testing.T) {
	resp, _, err := streamOpenAI(t, ""+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
		"data: [DONE]\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID == "" {
		t.Fatalf("tool calls = %+v, want one call with a non-empty id", resp.ToolCalls)
	}
}

// When the output limit cuts a tool call off mid-arguments, the arguments
// that arrived are incomplete: a write's content or an edit's new text is
// only partly there. Closing the JSON and running the tool wrote the
// truncated text to disk. The call must come back as a parse error instead.
func TestOpenAIStreamDoesNotRunToolCallTruncatedByLength(t *testing.T) {
	resp, _, err := streamOpenAI(t, ""+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"w\",\"function\":{\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\",\\\"content\\\":\\\"package main\\\\nfunc\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"+
		"data: [DONE]\n\n")
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if len(resp.ToolCalls) != 1 || !resp.ToolCalls[0].ParseError {
		t.Fatalf("tool calls = %+v, want one call flagged ParseError", resp.ToolCalls)
	}
	if _, leaked := resp.ToolCalls[0].Input["content"]; leaked {
		t.Fatalf("truncated content must not reach the tool: %#v", resp.ToolCalls[0].Input)
	}
}

// The non-streaming path always reported "stop", so an answer cut off by the
// output limit was never continued by the engine.
func TestOpenAIChatMapsFinishReasonLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"deepseek-v4-pro","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"part"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "k", baseURL: server.URL, client: server.Client()}
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-pro", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if resp.StopReason != "length" {
		t.Fatalf("StopReason = %q, want length", resp.StopReason)
	}
}

// DeepSeek ends a stream with finish_reason "insufficient_system_resource"
// when it aborts inference under load. That half answer used to be accepted
// as a finished turn.
func TestOpenAIStreamTreatsInsufficientSystemResourceAsFailure(t *testing.T) {
	_, _, err := streamOpenAI(t, ""+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"insufficient_system_resource\"}]}\n\n"+
		"data: [DONE]\n\n")
	if err == nil {
		t.Fatal("expected an error for an answer the server aborted")
	}
	if !isTemporary(err) {
		t.Fatalf("error %v should be classified temporary", err)
	}
}

func TestOpenAIChatRetriesInsufficientSystemResource(t *testing.T) {
	fastRetry(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		finish := "insufficient_system_resource"
		if calls > 1 {
			finish = "stop"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"`+finish+`","message":{"role":"assistant","content":"x"}}]}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "k", baseURL: server.URL, client: server.Client()}
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-pro", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if calls != 2 || resp.StopReason != "stop" {
		t.Fatalf("calls = %d, stop = %q; want a retry that completes", calls, resp.StopReason)
	}
}
