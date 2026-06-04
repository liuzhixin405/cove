package guardrail

import "testing"

func TestTrackerWarnsAndBlocksRepeatedExactFailures(t *testing.T) {
	tracker := New()
	args := map[string]any{"filePath": "missing.txt"}

	for i := 0; i < 2; i++ {
		tracker.AfterCall("read", args, "not found", true)
	}
	if got := tracker.BeforeCall("read", args); got.Action != Warn {
		t.Fatalf("BeforeCall after repeated failures = %v, want Warn", got.Action)
	}

	for i := 0; i < 3; i++ {
		tracker.AfterCall("read", args, "not found", true)
	}
	if got := tracker.BeforeCall("read", args); got.Action != Block {
		t.Fatalf("BeforeCall after five failures = %v, want Block", got.Action)
	}
}

func TestTrackerSuccessResetsFailureCounters(t *testing.T) {
	tracker := New()
	args := map[string]any{"pattern": "needle"}

	for i := 0; i < 3; i++ {
		tracker.AfterCall("grep", args, "error", true)
	}
	if got := tracker.BeforeCall("grep", args); got.Action != Warn {
		t.Fatalf("BeforeCall after failures = %v, want Warn", got.Action)
	}

	tracker.AfterCall("grep", args, "match", false)
	if got := tracker.BeforeCall("grep", args); got.Action != Allow {
		t.Fatalf("BeforeCall after success = %v, want Allow", got.Action)
	}
}

func TestTrackerBlocksRepeatedIdempotentResults(t *testing.T) {
	tracker := New()
	args := map[string]any{"filePath": "same.txt"}

	for i := 0; i < 3; i++ {
		tracker.AfterCall("read", args, "same result", false)
	}
	if got := tracker.BeforeCall("read", args); got.Action != Warn {
		t.Fatalf("BeforeCall after repeated identical results = %v, want Warn", got.Action)
	}

	for i := 0; i < 3; i++ {
		tracker.AfterCall("read", args, "same result", false)
	}
	if got := tracker.BeforeCall("read", args); got.Action != Block {
		t.Fatalf("BeforeCall after many identical results = %v, want Block", got.Action)
	}
}
