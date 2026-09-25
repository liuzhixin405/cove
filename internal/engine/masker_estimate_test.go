package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/token"
)

// The masker measured outputs as len/4, which reads 20000 Chinese
// characters (60000 bytes, ~20000 tokens) as 15000: Chinese output was
// protected and pruned against thresholds a quarter too small.
func TestMaskerCountsTokensWithTheSharedEstimator(t *testing.T) {
	m := NewToolOutputMasker()
	m.outputDir = t.TempDir()
	m.protectionThreshold = 1000
	old := strings.Repeat("中", 20000) // Estimate 20000, len/4 15000
	if token.Estimate(old) != 20000 {
		t.Fatalf("fixture estimate = %d", token.Estimate(old))
	}
	// Prunable only when counted with Estimate.
	m.minPrunableThreshold = 18000
	history := []api.Message{
		{Role: "user", Content: "read it"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "1", Name: "read"}}},
		{Role: "tool", ToolCallID: "1", Name: "read", Content: old},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "2", Name: "read"}}},
		{Role: "tool", ToolCallID: "2", Name: "read", Content: strings.Repeat("新", 2000)},
	}
	res, out := m.Mask(history, nil)
	if res.MaskedCount != 1 {
		t.Fatalf("masked %d outputs, want 1 (20000 CJK tokens >= 18000)", res.MaskedCount)
	}
	if res.TokensSaved != 20000 {
		t.Fatalf("TokensSaved = %d, want 20000", res.TokensSaved)
	}
	if !strings.Contains(out[2].Content, fmt.Sprintf("%d tokens masked", 20000)) {
		t.Fatalf("placeholder = %q", out[2].Content)
	}
}
