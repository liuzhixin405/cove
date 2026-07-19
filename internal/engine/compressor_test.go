package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// buildConversation returns a realistic agent transcript: a first user turn,
// then alternating assistant(tool_use) / tool(result) rounds, with some plain
// assistant text turns. Crucially there is NO leading system message — in Cove
// the system prompt is carried separately on ChatRequest.SystemBase.
func buildConversation() []api.Message {
	pad := strings.Repeat("x", 400) // keep each message well above the trim threshold
	msgs := []api.Message{
		{Role: "user", Content: "Please refactor the auth module. " + pad},
	}
	for i := 0; i < 7; i++ {
		msgs = append(msgs,
			api.Message{Role: "assistant", Content: "Working on step. " + pad, ToolCalls: []api.ToolCall{{ID: "t", Name: "read", Input: map[string]any{"filePath": "a.go"}}}},
			api.Message{Role: "tool", ToolCallID: "t", Name: "read", Content: "file contents " + pad},
		)
	}
	return msgs
}

// assertValidSequence enforces the model-API invariants that a malformed
// compression would violate: first turn is user, no two consecutive user turns,
// and no tool result orphaned from its assistant tool_use.
func assertValidSequence(t *testing.T, msgs []api.Message) {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("empty message list")
	}
	if msgs[0].Role != "user" {
		t.Fatalf("first message must be user, got %q", msgs[0].Role)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == "user" && msgs[i-1].Role == "user" {
			t.Fatalf("consecutive user turns at %d/%d — API rejects this with a 400", i-1, i)
		}
		if msgs[i].Role == "tool" && msgs[i-1].Role != "tool" && msgs[i-1].Role != "assistant" {
			t.Fatalf("orphaned tool_result at %d (preceded by %q)", i, msgs[i-1].Role)
		}
	}
}

func TestCompressor_ProducesValidSequence(t *testing.T) {
	cc := NewChatCompressor()
	msgs := buildConversation()
	tokens := countTokens(msgs)
	tokenLimit := tokens // threshold is 50% of the limit, so this forces compression

	// Long enough, and mentions the file the conversation actually touched
	// (a.go), so it passes validateSummaryQuality's coverage check — a
	// generic one-liner like "Summary of prior work." would now be
	// correctly rejected by that check and fall back to truncation instead.
	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "The user asked to refactor the auth module. The assistant repeatedly read a.go and inspected its contents across several steps. No errors were encountered; the task is still in progress."}, nil
	}

	result, out := cc.Compress(context.Background(), msgs, tokens, tokenLimit, stub)
	if !result.Compressed {
		t.Fatalf("expected compression to run (tokens=%d limit=%d)", tokens, tokenLimit)
	}
	assertValidSequence(t, out)

	// The first message must be the summary (a single user turn), not the
	// original first user message followed by a second summary user message.
	if !strings.Contains(out[0].Content, "<compress summary=\"conversation-history\">") {
		t.Fatalf("first message should be the summary, got %q", out[0].Content)
	}
	if out[1].Role != "assistant" {
		t.Fatalf("tail should be anchored on an assistant turn, got %q", out[1].Role)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("compression did not shrink history: %d -> %d", len(msgs), len(out))
	}
}

// When the summary model call fails, the truncation fallback must produce the
// same valid sequence — this path also previously kept messages[0] and inserted
// a second user turn.
func TestCompressor_FallbackTruncationValid(t *testing.T) {
	cc := NewChatCompressor()
	msgs := buildConversation()
	tokens := countTokens(msgs)

	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return nil, context.DeadlineExceeded
	}

	result, out := cc.Compress(context.Background(), msgs, tokens, tokens, stub)
	if !result.Compressed {
		t.Fatal("expected fallback truncation to mark Compressed")
	}
	assertValidSequence(t, out)
	if out[0].Role != "user" || !strings.Contains(out[0].Content, "truncated") {
		t.Fatalf("fallback should lead with a single truncation user turn, got %q", out[0].Content)
	}
}

// TestCompressor_RejectsLowQualitySummary is the direct regression test for
// §2.4: a shallow/generic summary that never mentions any file the
// conversation actually touched must be rejected and fall back to plain
// truncation, rather than silently becoming the new (unreliable) history.
func TestCompressor_RejectsLowQualitySummary(t *testing.T) {
	cc := NewChatCompressor()
	msgs := buildConversation()
	tokens := countTokens(msgs)
	tokenLimit := tokens

	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "Summary of prior work."}, nil // too generic, mentions no files
	}

	result, out := cc.Compress(context.Background(), msgs, tokens, tokenLimit, stub)
	if !result.Compressed {
		t.Fatalf("expected fallback truncation to still mark Compressed")
	}
	assertValidSequence(t, out)
	if strings.Contains(out[0].Content, "<compress summary=\"conversation-history\">") {
		t.Fatalf("low-quality summary should have been rejected in favor of truncation, got %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "truncated") {
		t.Fatalf("expected truncation fallback message, got %q", out[0].Content)
	}
}

func TestCompressor_BelowThresholdNoLayer2(t *testing.T) {
	cc := NewChatCompressor()
	msgs := buildConversation()
	called := false
	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		called = true
		return &api.ChatResponse{Content: "unused"}, nil
	}
	// Huge limit → well below the 50% Layer-2 trigger. Layer-1 tool-output
	// trimming may still run, but the expensive AI summarization (Layer 2) must
	// not: no summary call, no message dropped, roles unchanged.
	_, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 10_000_000, stub)
	if called {
		t.Fatal("Layer-2 summary model should not be called below threshold")
	}
	if len(out) != len(msgs) {
		t.Fatalf("Layer 2 restructured history below threshold: %d -> %d", len(msgs), len(out))
	}
	for i := range out {
		if out[i].Role != msgs[i].Role {
			t.Fatalf("role changed at %d below threshold: %q -> %q", i, msgs[i].Role, out[i].Role)
		}
	}
}
