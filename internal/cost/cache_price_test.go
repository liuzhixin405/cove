package cost

import (
	"math"
	"testing"
)

// Writing to Anthropic's prompt cache costs 1.25x the input price; those
// tokens were billed as plain input, so every turn that (re)built the cache
// was under-reported by a quarter of its prompt cost.
func TestCacheCreationBilledAtOneAndAQuarterInput(t *testing.T) {
	const model = "claude-sonnet-4-6"
	write := NewTracker(0)
	write.AddWithCacheWrite(model, 1000, 0, 0, 1000, 1000)
	plain := NewTracker(0)
	plain.AddDetailed(model, 1250, 0, 0, 1250)

	got, want := write.Totals().Cost, plain.Totals().Cost
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("1000 cache-creation tokens cost %.10f, want %.10f (1250 input tokens)", got, want)
	}
	if write.Totals().Input != 1000 {
		t.Fatalf("input tokens = %d, want 1000 (cache writes are still input)", write.Totals().Input)
	}
}

// Without cache writes the new entry point bills exactly like AddDetailed.
func TestAddWithCacheWriteZeroMatchesAddDetailed(t *testing.T) {
	const model = "claude-opus-4-7"
	a := NewTracker(0)
	a.AddWithCacheWrite(model, 5000, 700, 3000, 2000, 0)
	b := NewTracker(0)
	b.AddDetailed(model, 5000, 700, 3000, 2000)
	if a.Totals() != b.Totals() {
		t.Fatalf("totals differ: %+v vs %+v", a.Totals(), b.Totals())
	}
}
