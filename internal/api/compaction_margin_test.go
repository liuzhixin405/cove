package api

import "testing"

// A flat 8000-token margin put a 16384 window's trigger at 4288, below the
// ~6.5K fixed overhead of every request, and compaction was switched off
// although ~5.7K tokens were left for the conversation.
func TestSmallWindowTriggerClearsTheRequestOverhead(t *testing.T) {
	t.Cleanup(ClearModelContextWindows)
	SetModelContextWindow("qwen3.6-27b", 16384)
	const overhead, minHistory = 6536, 2000
	trigger := CompactionTrigger("qwen3.6-27b")
	if trigger <= overhead+minHistory {
		t.Fatalf("trigger %d leaves no history room above a %d overhead", trigger, overhead)
	}
	if reply := MaxOutputTokensForModel("qwen3.6-27b"); trigger+reply > 16384 {
		t.Fatalf("trigger %d + reply %d overflow the window", trigger, reply)
	}
}

// Windows of 64K and up keep the full margin they always had.
func TestCompactionSafetyMarginScalesWithTheWindow(t *testing.T) {
	t.Cleanup(ClearModelContextWindows)
	for window, want := range map[int]int{4096: 1024, 8192: 1024, 16384: 2048, 32768: 4096, 64000: 8000, 1000000: 8000} {
		SetModelContextWindow("margin-test-model", window)
		if got := CompactionSafetyMarginFor("margin-test-model"); got != want {
			t.Errorf("window %d: margin %d, want %d", window, got, want)
		}
	}
}
