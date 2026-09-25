package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/token"
)

// 200K-window model used by the compaction tests below.
const tokenCountTestModel = "claude-sonnet-4-20250514"

// The provider's reported prompt size is the real context size. The old
// bytes/4 estimate undercounted Chinese text three-fold and ignored the
// system prompt and tool definitions entirely, and the threshold (half the
// budget) compacted long before the window was anywhere near full.
func TestCompactionFollowsReportedInputTokens(t *testing.T) {
	if w := api.ContextWindowForModel(tokenCountTestModel); w != 200000 {
		t.Fatalf("test model window = %d, want 200000", w)
	}
	threshold := compactionThreshold(tokenCountTestModel)

	eng := newTestEngine(&mockProvider{})
	eng.messages = []api.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	chinese := strings.Repeat("中", 1000)

	eng.recordUsage(100000, len(eng.messages))
	eng.messages = append(eng.messages, api.Message{Role: "user", Content: chinese})
	eng.updateTokenCount()
	if eng.totalTokens < 100000+1000 {
		t.Fatalf("totalTokens = %d, want reported 100000 + ~1000 for the new message", eng.totalTokens)
	}
	if eng.compressor.NeedsCompression(eng.totalTokens, threshold) {
		t.Fatalf("100K of a 200K window triggered compaction (tokens=%d threshold=%d)", eng.totalTokens, threshold)
	}

	// 128K of a 200K window leaves too little room for the reply
	// (MaxTokens plus a safety margin), so it compacts.
	eng.messages = eng.messages[:2]
	eng.recordUsage(128000, len(eng.messages))
	eng.updateTokenCount()
	if !eng.compressor.NeedsCompression(eng.totalTokens, threshold) {
		t.Fatalf("128K of a 200K window did not trigger compaction (tokens=%d threshold=%d)", eng.totalTokens, threshold)
	}

	eng.recordUsage(170000, len(eng.messages))
	eng.messages = append(eng.messages, api.Message{Role: "user", Content: chinese})
	eng.updateTokenCount()
	if !eng.compressor.NeedsCompression(eng.totalTokens, threshold) {
		t.Fatalf("170K of a 200K window did not trigger compaction (tokens=%d threshold=%d)", eng.totalTokens, threshold)
	}
}

// Without a provider report (first request, or history rewritten since), the
// count falls back to the shared token estimator over everything sent:
// system prompt, tool definitions, messages and reasoning.
func TestTokenCountFallsBackToEstimateOfEverythingSent(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	sp := strings.Repeat("system rules ", 200)
	tools := []api.ToolDef{{Name: "read", Description: strings.Repeat("reads a file ", 100)}}
	eng.setRequestOverhead(sp, tools)
	eng.messages = []api.Message{
		{Role: "user", Content: strings.Repeat("中文", 500)},
		{Role: "assistant", Content: "ok", ReasoningContent: strings.Repeat("思考", 500)},
	}
	eng.updateTokenCount()

	msgs := token.Estimate(eng.messages[0].Content) + token.Estimate(eng.messages[1].ReasoningContent)
	floor := token.Estimate(sp) + token.Estimate(tools[0].Description) + msgs
	if eng.totalTokens < floor {
		t.Fatalf("totalTokens = %d, want at least %d (system + tools + messages + reasoning)", eng.totalTokens, floor)
	}
	// 1000 CJK runes are ~1000 tokens, not the 750 that bytes/4 of 3000 bytes gave.
	if n := countTokens(eng.messages[:1]); n < 1000 {
		t.Fatalf("countTokens(1000 CJK runes) = %d, want >= 1000", n)
	}
}

// Rewriting earlier history invalidates the provider's figure: it measured a
// prefix that no longer exists.
func TestRewrittenHistoryDropsTheReportedTokenAnchor(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	eng.messages = []api.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}
	eng.recordUsage(150000, 2)
	eng.LoadMessages([]api.Message{{Role: "user", Content: "fresh"}})
	if eng.totalTokens >= 150000 {
		t.Fatalf("totalTokens = %d after LoadMessages; stale provider figure kept", eng.totalTokens)
	}
}

// A live turn records the provider's figure.
func TestTurnRecordsReportedInputTokens(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{{content: "answer", inputTokens: 4321}}}
	eng := newTestEngine(prov)
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "q"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if eng.lastInputTokens != 4321 {
		t.Fatalf("lastInputTokens = %d, want 4321", eng.lastInputTokens)
	}
	eng.updateTokenCount()
	if eng.totalTokens < 4321 {
		t.Fatalf("totalTokens = %d, want >= reported 4321", eng.totalTokens)
	}
}

// The trigger always leaves room for the reply: window minus the request's
// MaxTokens minus a safety margin. A 64K-window model compacted at ~43.5K
// and then asked for 64K of output it could never get.
func TestCompactionLeavesRoomForTheReply(t *testing.T) {
	for _, model := range []string{"deepseek-chat", tokenCountTestModel, "gpt-4o", "claude-opus-5", "qwen-turbo"} {
		window := api.ContextWindowForModel(model)
		maxOut := api.MaxOutputTokensForModel(model)
		trigger := compactionThreshold(model)
		if trigger > window-maxOut-compactionSafetyMargin {
			t.Errorf("%s: trigger %d leaves less than MaxTokens %d + %d of its %d window", model, trigger, maxOut, compactionSafetyMargin, window)
		}
	}
	const small = "deepseek-chat" // 64K window
	if w := api.ContextWindowForModel(small); w != 64000 {
		t.Fatalf("%s window = %d, want 64000", small, w)
	}
	if m := api.MaxOutputTokensForModel(small); m > 16000 {
		t.Fatalf("%s MaxTokens = %d, want <= 16000", small, m)
	}
	if tr := compactionThreshold(small); tr > 64000-16000-8000 {
		t.Fatalf("%s trigger = %d, want <= %d", small, tr, 64000-16000-8000)
	}
}

// The request asks for no more output than the model's window allows.
func TestRequestMaxTokensFollowsTheModel(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{{content: "ok"}}}
	eng := newTestEngine(prov)
	eng.config.Model = "deepseek-chat"
	eng.modelRouter = nil
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "q"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := prov.lastReq.MaxTokens; got > 16000 || got < 4000 {
		t.Fatalf("MaxTokens for a 64K-window model = %d, want 4000..16000", got)
	}
}
