package cost

import (
	"math"
	"testing"
)

// DeepSeek V4 list prices per MTok (api-docs.deepseek.com, models & pricing).
// The tracker bills at the peak rate — off-peak is exactly half — so the
// max_budget_usd cap is never exceeded by accident. The table previously
// billed deepseek-v4-pro at $0.14 / $0.28, about a tenth of the real price.
func TestDeepSeekV4ModelsAreBilledAtPeakListPrice(t *testing.T) {
	cases := []struct {
		model             string
		in, out, cacheHit int
		want              float64
	}{
		{"deepseek-v4-pro", 1_000_000, 1_000_000, 0, 1.32 + 3.96},
		{"deepseek-flash", 1_000_000, 1_000_000, 0, 0.30 + 1.20},
		{"deepseek-v4-flash", 1_000_000, 1_000_000, 0, 0.30 + 1.20}, // legacy name, Flash pricing
		{"deepseek-v4-pro", 1_000_000, 0, 1_000_000, 0.044},         // all input served from cache
		{"deepseek-flash", 1_000_000, 0, 1_000_000, 0.006},
	}
	for _, tc := range cases {
		tr := NewTracker(0)
		tr.AddDetailed(tc.model, tc.in, tc.out, tc.cacheHit, 0)
		if got := tr.Totals().Cost; math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s in=%d out=%d hit=%d: cost = %.4f, want %.4f", tc.model, tc.in, tc.out, tc.cacheHit, got, tc.want)
		}
	}
}
