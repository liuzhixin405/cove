package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// A reasoning model spends part of max_tokens thinking before it answers. At
// 600 the compaction summary of deepseek-v4-pro could come back empty, and an
// empty summary is rejected, so compaction silently never happened.
func TestSummaryRequestLeavesRoomForReasoning(t *testing.T) {
	var got api.ChatRequest
	tryChat := func(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
		got = req
		return &api.ChatResponse{Content: "summary"}, nil
	}
	msgs := []api.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}}
	if _, err := (&ChatCompressor{}).generateSummary(context.Background(), msgs, tryChat); err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens < 2000 {
		t.Fatalf("summary MaxTokens = %d, too small for a reasoning model", got.MaxTokens)
	}
}
