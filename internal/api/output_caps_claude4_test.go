package api

import "testing"

// Opus 4 and Opus 4.1 accept at most 32000 output tokens. They were missing
// from modelOutputCaps, so their 200K window gave an outputReserve of 50000
// and every request was rejected with a 400. Sonnet 4/4.5 and Opus 4.5 take
// 64000, which is the request ceiling anyway; they are listed so the table
// says so rather than relying on the clamp.
func TestMaxOutputTokensForClaude4Generation(t *testing.T) {
	cases := []struct {
		model string
		max   int // MaxOutputTokensForModel must not exceed this
	}{
		{"claude-opus-4-1-20250805", 32000},
		{"claude-opus-4-1", 32000},
		{"claude-opus-4-20250514", 32000},
		{"claude-opus-4-0", 32000},
		{"claude-opus-4@20250514", 32000},                  // Vertex ID
		{"anthropic.claude-opus-4-1-20250805-v1:0", 32000}, // Bedrock ID
		{"claude-sonnet-4-20250514", 64000},
		{"claude-sonnet-4-5-20250929", 64000},
		{"claude-opus-4-5-20251101", 64000},
	}
	for _, tc := range cases {
		if got := MaxOutputTokensForModel(tc.model); got > tc.max {
			t.Errorf("MaxOutputTokensForModel(%q) = %d, want <= %d", tc.model, got, tc.max)
		}
	}
}

// The newer Opus models share the "claude-opus-4" prefix but not the 32K
// cap; matching them against the Opus 4 entry would halve their output.
func TestOpus4CapDoesNotShadowNewerOpusModels(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8-20260101"} {
		if got := MaxOutputTokensForModel(model); got != maxRequestOutputTokens {
			t.Errorf("MaxOutputTokensForModel(%q) = %d, want %d (no Opus 4 cap)", model, got, maxRequestOutputTokens)
		}
	}
}
