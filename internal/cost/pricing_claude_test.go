package cost

import (
	"math"
	"testing"
)

// Current Claude models were missing from the price table and fell through to
// defaultPrice ($0.435 / $0.87 per MTok) — an order of magnitude below their
// real rates — so spend was under-reported and max_budget_usd never tripped.
// Expected values are the published per-MTok list prices (input, output).
func TestCurrentClaudeModelsAreBilledAtListPrice(t *testing.T) {
	cases := []struct {
		model string
		want  float64 // cost of 1M input + 1M output tokens
	}{
		{"claude-fable-5-1", 10 + 50},
		{"claude-opus-5", 5 + 25},
		{"claude-opus-4-8", 5 + 25},
		{"claude-opus-4-6", 5 + 25},
		{"claude-sonnet-5", 2 + 10},
		{"claude-sonnet-4-6", 3 + 15},
		{"claude-haiku-4-5", 1 + 5},
		{"anthropic.claude-opus-5", 5 + 25}, // Bedrock-style prefixed ID
	}
	for _, tc := range cases {
		tr := NewTracker(0)
		tr.AddDetailed(tc.model, 1_000_000, 1_000_000, 0, 0)
		if got := tr.Totals().Cost; math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: cost = %.4f, want %.4f", tc.model, got, tc.want)
		}
	}
}
