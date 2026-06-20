package cost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrackerAddDetailedUsesDeepSeekCacheHitPricing(t *testing.T) {
	tracker := NewTracker(0)
	tracker.AddDetailed("deepseek-v4-pro", 1000, 200, 600, 400)

	if tracker.TotalPromptCacheHit != 600 {
		t.Fatalf("TotalPromptCacheHit = %d, want 600", tracker.TotalPromptCacheHit)
	}
	if tracker.TotalPromptCacheMiss != 400 {
		t.Fatalf("TotalPromptCacheMiss = %d, want 400", tracker.TotalPromptCacheMiss)
	}

	want := (400.0/1e6)*0.14 + (600.0/1e6)*(0.14*0.1) + (200.0/1e6)*0.28
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
	if got != "9836 in (cache hit 1280, miss 8556) | 54 out | $0.0012 / $10.00" {
		t.Fatalf("Summary() = %q", got)
	}
}

func TestCostHistoryRecordsLoadErrorForInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	h := &CostHistory{path: filepath.Join(dir, "cost_history.json")}
	if err := os.WriteFile(h.path, []byte(`{"records":`), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}

	h.load()
	if h.LoadError == nil {
		t.Fatal("expected load error for invalid JSON")
	}
	if len(h.Records) != 0 {
		t.Fatalf("expected no records after invalid JSON, got %#v", h.Records)
	}
}
