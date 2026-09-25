package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// A limit of 0 forces the summary layer (/compact): layer-1 trimming alone
// used to count as "compressed" and /compact reported success for a history
// it barely touched.
func TestCompressForceRunsSummaryLayer(t *testing.T) {
	cc := NewChatCompressor()
	msgs := buildConversation()
	called := false
	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		called = true
		return &api.ChatResponse{Content: "Refactored auth: read a.go several times; next step is the tests."}, nil
	}
	res, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 0, stub)
	if !called || !res.Summarized {
		t.Fatalf("forced compression did not summarize: called=%v res=%+v", called, res)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("history not shortened: %d -> %d", len(msgs), len(out))
	}
	assertValidSequence(t, out)
}

// /compact works from 4 messages (it needed 12).
func TestCompressForceWorksOnFourMessages(t *testing.T) {
	cc := NewChatCompressor()
	pad := strings.Repeat("y", 300)
	msgs := []api.Message{
		{Role: "user", Content: "first request " + pad},
		{Role: "assistant", Content: "answer one " + pad},
		{Role: "user", Content: "second request " + pad},
		{Role: "assistant", Content: "answer two " + pad},
	}
	stub := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "The user asked two things; both were answered in full."}, nil
	}
	res, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 0, stub)
	if !res.Summarized {
		t.Fatalf("4 messages not summarized: %+v", res)
	}
	assertValidSequence(t, out)
	// Not forced: still needs 12 messages.
	res2, _ := NewChatCompressor().Compress(context.Background(), msgs, countTokens(msgs), 1, stub)
	if res2.Compressed {
		t.Fatalf("automatic compression ran on 4 messages: %+v", res2)
	}
	if res3, _ := NewChatCompressor().Compress(context.Background(), msgs[:3], countTokens(msgs[:3]), 0, stub); res3.Compressed || res3.Reason == "" {
		t.Fatalf("3 messages: want a reason and no compression, got %+v", res3)
	}
}

// The summary input keeps the original request (first real user message)
// up to 2000 characters, later user messages 600, assistant 250, tool 100.
func TestSummaryInputKeepsOriginalRequest(t *testing.T) {
	cc := NewChatCompressor()
	first := strings.Repeat("需", 1900) + "FIRST-END"
	later := strings.Repeat("u", 590) + "LATER-END"
	msgs := []api.Message{
		{Role: "user", Content: "[system: injected note]", Synthetic: true},
		{Role: "user", Content: first},
		{Role: "assistant", Content: strings.Repeat("a", 240) + "ASSIST-END" + strings.Repeat("a", 100)},
		{Role: "user", Content: later},
		{Role: "tool", Content: strings.Repeat("t", 200)},
	}
	var input string
	stub := func(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
		input = req.Messages[0].Content
		return &api.ChatResponse{Content: "ok"}, nil
	}
	if _, err := cc.generateSummary(context.Background(), msgs, stub); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FIRST-END", "LATER-END", "ASSIST-END"} {
		if !strings.Contains(input, want) {
			t.Errorf("summary input lost %s", want)
		}
	}
	if strings.Contains(input, strings.Repeat("t", 101)) {
		t.Error("tool output kept beyond 100 characters")
	}
	if strings.Contains(input, strings.Repeat("a", 251)) {
		t.Error("assistant text kept beyond 250 characters")
	}
}

// Engine.Compact reports the token counts before and after, and says why
// when nothing could be compressed.
func TestEngineCompactReportsTokens(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{{content: "The user asked for many words and got them; nothing else is pending."}}}
	eng := newTestEngine(prov)
	eng.messages = longHistory(20)
	eng.updateTokenCount()
	rep := eng.Compact(context.Background())
	if !rep.Compressed || !rep.Summarized || rep.BeforeTokens <= rep.AfterTokens || rep.AfterTokens <= 0 {
		t.Fatalf("report: %+v", rep)
	}

	eng2 := newTestEngine(&mockProvider{})
	eng2.messages = longHistory(2)
	rep2 := eng2.Compact(context.Background())
	if rep2.Compressed || rep2.Reason == "" {
		t.Fatalf("short history: want a reason, got %+v", rep2)
	}
}

// Automatic compaction tells the user in one line.
func TestAutoCompactionPrintsNotice(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{{content: "The user asked for many words and got them; nothing else is pending."}}}
	eng := newTestEngine(prov)
	var lines []string
	eng.OnEngineOutput = func(s string) { lines = append(lines, s) }
	eng.messages = longHistory(20)
	eng.updateTokenCount()
	eng.compactIfNeeded(context.Background(), 1)
	if !strings.Contains(strings.Join(lines, "\n"), "已压缩上下文") {
		t.Fatalf("no compaction notice: %q", lines)
	}
}
