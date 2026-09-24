package adapter

import "testing"

// A long DeepSeek answer arrives as tens of thousands of tiny deltas.
// Appending each one to a string copied the whole text every time
// (quadratic: gigabytes of copying for one long reasoning trace).
func TestStreamAccumulatorAppendsWithoutCopyingPerDelta(t *testing.T) {
	const n = 20000
	allocs := testing.AllocsPerRun(1, func() {
		var acc StreamAccumulator
		for i := 0; i < n; i++ {
			acc.AddDelta("ab")
			acc.AddReasoning("cd")
		}
		if len(acc.Content()) != 2*n || len(acc.Reasoning()) != 2*n {
			t.Fatalf("lengths = %d, %d", len(acc.Content()), len(acc.Reasoning()))
		}
	})
	if allocs > 200 {
		t.Fatalf("%v allocations for %d deltas, want amortized growth", allocs, 2*n)
	}
}
