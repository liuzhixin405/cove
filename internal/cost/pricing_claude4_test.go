package cost

import (
	"math"
	"testing"
)

// The Claude 4 / 4.1 / 4.5 generation (Sonnet 4, Sonnet 4.5, Opus 4, Opus 4.1,
// Opus 4.5) was missing from Prices and fell to defaultPrice ($0.435 / $0.87
// per MTok), under-billing them 7-35x and leaving max_budget_usd toothless.
// Expected values are the published per-MTok list prices, looked up by the
// dated IDs the API actually returns.
func TestClaude4GenerationIsBilledAtListPrice(t *testing.T) {
	cases := []struct {
		model string
		want  float64 // cost of 1M input + 1M output tokens
	}{
		{"claude-sonnet-4-20250514", 3 + 15},
		{"claude-sonnet-4-5-20250929", 3 + 15},
		{"claude-opus-4-20250514", 15 + 75},
		{"claude-opus-4-1-20250805", 15 + 75},
		{"claude-opus-4-5-20251101", 5 + 25},
		{"anthropic.claude-opus-4-1-20250805-v1:0", 15 + 75}, // Bedrock-style prefixed ID
		// The neighbours must keep their own rates: "claude-opus-4" is a
		// substring of these and the longest key has to win.
		{"claude-opus-4-6", 5 + 25},
		{"claude-sonnet-4-6", 3 + 15},
	}
	for _, tc := range cases {
		tr := NewTracker(0)
		tr.AddDetailed(tc.model, 1_000_000, 1_000_000, 0, 0)
		if got := tr.Totals().Cost; math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: cost = %.4f, want %.4f", tc.model, got, tc.want)
		}
	}
}

// Cache reads on the new entries follow the table's convention (0.1x input),
// so a cached prompt is not billed at the full input rate.
func TestClaude4GenerationCacheReadRate(t *testing.T) {
	tr := NewTracker(0)
	tr.AddDetailed("claude-opus-4-1-20250805", 1_000_000, 0, 1_000_000, 0)
	if got := tr.Totals().Cost; math.Abs(got-1.5) > 1e-9 {
		t.Fatalf("1M cached input on opus-4-1 = %.4f, want 1.50 (0.1x of $15)", got)
	}
}

// An unknown future claude-* ID must over-count, not under-count: it is
// billed at the most expensive known Claude tier so max_budget_usd stays a
// real ceiling. Non-Claude unknowns keep the cheap defaultPrice.
func TestUnknownClaudeModelFallsToMostExpensiveClaudeTier(t *testing.T) {
	tr := NewTracker(0)
	tr.AddDetailed("claude-zeta-9-20270101", 1_000_000, 1_000_000, 0, 0)
	if got := tr.Totals().Cost; math.Abs(got-(15+75)) > 1e-9 {
		t.Fatalf("unknown claude model = %.4f, want %.4f (Opus list price)", got, 15.0+75.0)
	}

	other := NewTracker(0)
	other.AddDetailed("totally-unknown-model-x", 1_000_000, 1_000_000, 0, 0)
	if got, want := other.Totals().Cost, defaultPrice.Input+defaultPrice.Output; math.Abs(got-want) > 1e-9 {
		t.Fatalf("unknown non-claude model = %.4f, want defaultPrice %.4f", got, want)
	}
}
