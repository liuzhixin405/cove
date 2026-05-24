package cost

import "testing"

func TestTrackerAddDetailedUsesDeepSeekCacheHitPricing(t *testing.T) {
	tracker := NewTracker(0)
	tracker.AddDetailed("deepseek-v4-pro", 1000, 200, 600, 400)

	if tracker.TotalPromptCacheHit != 600 {
		t.Fatalf("TotalPromptCacheHit = %d, want 600", tracker.TotalPromptCacheHit)
	}
	if tracker.TotalPromptCacheMiss != 400 {
		t.Fatalf("TotalPromptCacheMiss = %d, want 400", tracker.TotalPromptCacheMiss)
	}

	want := (400.0/1e6)*0.435 + (600.0/1e6)*0.003625 + (200.0/1e6)*0.87
	if diff := tracker.TotalCost - want; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("TotalCost = %.12f, want %.12f", tracker.TotalCost, want)
	}
}

func TestTrackerAddDetailedBackfillsCacheMissWhenUsageOnlyProvidesHits(t *testing.T) {
	tracker := NewTracker(0)
	tracker.AddDetailed("deepseek-v4-flash", 100, 5, 40, 0)

	if tracker.TotalPromptCacheHit != 40 {
		t.Fatalf("TotalPromptCacheHit = %d, want 40", tracker.TotalPromptCacheHit)
	}
	if tracker.TotalPromptCacheMiss != 60 {
		t.Fatalf("TotalPromptCacheMiss = %d, want 60", tracker.TotalPromptCacheMiss)
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
	if got != "9836 in (cache hit 1280, miss 8556) | 54 out | $0.0038 / $10.00" {
		t.Fatalf("Summary() = %q", got)
	}
}
