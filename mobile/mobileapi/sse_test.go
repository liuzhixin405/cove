package mobileapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamServer serves the given SSE lines verbatim, each followed by a blank
// line.
func streamServer(t *testing.T, lines ...string) *streamTarget {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return &streamTarget{p: newOpenAICompatProvider(ProviderConfig{Name: "deepseek", APIKey: "k", BaseURL: srv.URL})}
}

type streamTarget struct{ p *openAICompatProvider }

func (s *streamTarget) run() (ChatResponse, error) {
	return s.p.ChatStream(context.Background(), ChatRequest{Model: "deepseek-chat"}, nil)
}

// TestChatStreamAcceptsDataWithoutSpace: the space after "data:" is optional
// in SSE and some OpenAI-compatible gateways leave it out; matching only
// "data: " skipped every line, so the answer came back empty.
func TestChatStreamAcceptsDataWithoutSpace(t *testing.T) {
	resp, err := streamServer(t,
		`data:{"choices":[{"delta":{"content":"he"}}]}`,
		`data:{"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}`,
		`data:[DONE]`,
	).run()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" {
		t.Fatalf("Content = %q, want hello", resp.Content)
	}
}

// TestChatStreamRejectsTruncatedStream: a connection that closed mid-answer —
// no finish_reason and no [DONE] — was returned as a complete answer.
func TestChatStreamRejectsTruncatedStream(t *testing.T) {
	_, err := streamServer(t,
		`data: {"choices":[{"delta":{"content":"the answer is"}}]}`,
	).run()
	if err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatalf("err = %v, want a truncated-stream error", err)
	}

	// finish_reason without [DONE] is a complete answer.
	resp, err := streamServer(t,
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
	).run()
	if err != nil || resp.Content != "ok" {
		t.Fatalf("finish_reason without [DONE]: %q, %v", resp.Content, err)
	}
}

// TestChatStreamReportsInStreamError: servers report failures after the 200
// header (upstream overload, moderation) as {"error":...} in the stream; it
// was skipped and the caller got an empty "success".
func TestChatStreamReportsInStreamError(t *testing.T) {
	_, err := streamServer(t,
		`data: {"error":{"message":"upstream overloaded","type":"server_error"}}`,
		`data: [DONE]`,
	).run()
	if err == nil || !strings.Contains(err.Error(), "upstream overloaded") {
		t.Fatalf("err = %v, want the stream error", err)
	}
}

// TestChatStreamSplitsParallelCallsSharingIndex: some servers send every
// parallel call whole at index 0 with its own id. Merging by index glued the
// argument objects together into one call whose JSON did not parse.
func TestChatStreamSplitsParallelCallsSharingIndex(t *testing.T) {
	resp, err := streamServer(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"tap","arguments":"{\"x\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"swipe","arguments":"{\"y\":2}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	).run()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2: %+v", len(resp.ToolCalls), resp.ToolCalls)
	}
	a, b := resp.ToolCalls[0], resp.ToolCalls[1]
	if a.ID != "a" || a.Name != "tap" || a.Input["x"] != float64(1) {
		t.Errorf("first call = %+v", a)
	}
	if b.ID != "b" || b.Name != "swipe" || b.Input["y"] != float64(2) {
		t.Errorf("second call = %+v", b)
	}
}

// TestChatStreamKeepsCallWithEmptyArguments: a no-argument tool streams
// "arguments":"" — the call must still be returned, with empty input.
func TestChatStreamKeepsCallWithEmptyArguments(t *testing.T) {
	resp, err := streamServer(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"s","function":{"name":"screenshot","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	).run()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "screenshot" || resp.ToolCalls[0].ID != "s" || resp.ToolCalls[0].Input == nil {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}
