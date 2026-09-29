package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// The original request travels with every compaction: a real session began
// with "<compress summary="context-truncated">" and nothing else, so the
// model continued a task it could no longer name.
func TestCompressor_TruncationFallbackKeepsTheOriginalRequest(t *testing.T) {
	cc := NewChatCompressor()
	msgs := buildConversation()
	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return nil, context.DeadlineExceeded
	}
	_, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 1, stub)
	assertValidSequence(t, out)
	if !strings.Contains(out[0].Content, "Please refactor the auth module.") {
		t.Fatalf("truncation dropped the original request:\n%s", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "<original_request>") {
		t.Fatalf("original request not marked as such:\n%s", out[0].Content)
	}
}

func TestCompressor_SummaryKeepsTheOriginalRequestVerbatim(t *testing.T) {
	cc := NewChatCompressor()
	msgs := buildConversation()
	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "The user asked to refactor the auth module. The assistant repeatedly read a.go and inspected its contents across several steps. No errors were encountered; the task is still in progress."}, nil
	}
	_, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 1, stub)
	assertValidSequence(t, out)
	if !strings.Contains(out[0].Content, "<original_request>\nPlease refactor the auth module.") {
		t.Fatalf("summary message lacks the verbatim original request:\n%s", out[0].Content)
	}
}

// A synthetic first message (a turn note, an interruption marker) is not
// the request; the first genuine user message is.
func TestCompressor_OriginalRequestSkipsSyntheticMessages(t *testing.T) {
	msgs := []api.Message{
		{Role: "user", Content: "[system: The previous turn was interrupted]", Synthetic: true},
		{Role: "user", Content: "把 README 翻译成英文"},
		{Role: "assistant", Content: "好"},
	}
	if got := originalRequest(msgs); got != "把 README 翻译成英文" {
		t.Fatalf("originalRequest = %q", got)
	}
	if got := originalRequest(nil); got != "" {
		t.Fatalf("originalRequest(nil) = %q", got)
	}
}
