package cost

import (
	"math"
	"testing"
)

// Dated model names are what the fuzzy price lookup exists for, and they are
// where it breaks: "gpt-4o-mini-2024-07-18" contains both "gpt-4o-mini" and the
// shorter "gpt-4o". Go randomizes map iteration order, so a first-match loop
// resolves that name to a different price on different runs — billing a mini
// call at 16x the real rate whenever the short key comes up first.
//
// The longest matching key is the most specific one and has to win.
func TestAddDetailedPicksTheMostSpecificPriceKey(t *testing.T) {
	cases := []struct {
		model string
		want  Price
	}{
		{"gpt-4o-mini-2024-07-18", Prices["gpt-4o-mini"]},
		{"gpt-4o-2024-08-06", Prices["gpt-4o"]},
		{"claude-3-5-sonnet-20241022", Prices["claude-3-5-sonnet"]},
		{"deepseek-v4-flash-0731", Prices["deepseek-v4-flash"]},
	}

	// One run picks the wrong key about half the time, which would make this a
	// coin flip rather than a test. Accumulating many runs turns a wrong pick
	// into a visible mismatch in the total.
	const runs = 200

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			var got float64
			for i := 0; i < runs; i++ {
				// 1M uncached input tokens => TotalCost is exactly p.Input.
				tr := NewTracker(0)
				tr.AddDetailed(tc.model, 1_000_000, 0, 0, 0)
				got += tr.Totals().Cost
			}
			want := float64(runs) * tc.want.Input
			if math.Abs(got-want) > 1e-6 {
				t.Errorf("%d runs of %s totalled $%.4f, want $%.4f: the fuzzy match picked the wrong price key",
					runs, tc.model, got, want)
			}
		})
	}
}

func TestTrackerAddDetailedUsesDeepSeekCacheHitPricing(t *testing.T) {
	tracker := NewTracker(0)
	tracker.AddDetailed("deepseek-v4-pro", 1000, 200, 600, 400)

	if tracker.Totals().PromptCacheHit != 600 {
		t.Fatalf("TotalPromptCacheHit = %d, want 600", tracker.Totals().PromptCacheHit)
	}
	if tracker.Totals().PromptCacheMiss != 400 {
		t.Fatalf("TotalPromptCacheMiss = %d, want 400", tracker.Totals().PromptCacheMiss)
	}

	want := (400.0/1e6)*0.14 + (600.0/1e6)*(0.14*0.1) + (200.0/1e6)*0.28
	if diff := tracker.Totals().Cost - want; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("TotalCost = %.12f, want %.12f", tracker.Totals().Cost, want)
	}
}

func TestTrackerAddDetailedBackfillsCacheMissWhenUsageOnlyProvidesHits(t *testing.T) {
	tracker := NewTracker(0)
	tracker.AddDetailed("deepseek-v4-flash", 100, 5, 40, 0)

	if tracker.Totals().PromptCacheHit != 40 {
		t.Fatalf("TotalPromptCacheHit = %d, want 40", tracker.Totals().PromptCacheHit)
	}
	if tracker.Totals().PromptCacheMiss != 60 {
		t.Fatalf("TotalPromptCacheMiss = %d, want 60", tracker.Totals().PromptCacheMiss)
	}
}

func TestTrackerSummaryIncludesCacheBreakdown(t *testing.T) {
	tracker := NewTracker(10)
	tracker.AddDetailed("deepseek-v4-flash", 100, 5, 40, 60)
	got := tracker.Summary()
	want := "100 in (cache hit 40, miss 60) | 5 out | $0.00 / $10.00"
	if got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func TestTrackerSummaryShowsSmallNonZeroCost(t *testing.T) {
	tracker := NewTracker(10)
	tracker.AddDetailed("deepseek-v4-pro", 9836, 54, 1280, 8556)
	got := tracker.Summary()
	if got != "9836 in (cache hit 1280, miss 8556) | 54 out | $0.0012 / $10.00" {
		t.Fatalf("Summary() = %q", got)
	}
}
