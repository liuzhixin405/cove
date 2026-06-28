package engine

import (
	"strings"
	"testing"
)

func TestLoopDetector_ToolCallRepeat(t *testing.T) {
	ld := NewLoopDetector()

	// Same fingerprint ×9 → no detection yet (fpThresh=10, toolOnlyThresh=10)
	for i := 0; i < 9; i++ {
		r := ld.RecordToolCalls("fp-abc")
		if r.Detected {
			t.Fatalf("unexpected detection at iteration %d", i)
		}
	}

	// 10th → L1b fires first (toolOnlyWindow=12, count=10/12=83%%)
	r := ld.RecordToolCalls("fp-abc")
	if !r.Detected {
		t.Fatal("expected detection on 10th repeat (L1b: 10/12)")
	}
	if r.Layer != 1 {
		t.Fatalf("expected layer 1, got %d", r.Layer)
	}
	if r.Fatal {
		t.Fatal("first detection should be non-fatal (guidance mode)")
	}
}

func TestLoopDetector_DifferentFingerprints_NoTrigger(t *testing.T) {
	ld := NewLoopDetector()
	for i := 0; i < 20; i++ {
		fp := "fp-" + string(rune('a'+i%10))
		r := ld.RecordToolCalls(fp)
		if r.Detected {
			t.Fatalf("unexpected detection: %s", r.Reason)
		}
	}
}

func TestLoopDetector_AlternatingPattern(t *testing.T) {
	ld := NewLoopDetector()
	// Pattern: A,A,A,B,A,A,A,B,... (A appears ~75% of the time)
	// With toolOnlyWindow=12, toolOnlyThresh=10, need 10 fp-A in last 12.
	// After 13 iterations of A,A,A,B pattern: 10 As -> L1b fires at iteration 12.
	for i := 0; i < 12; i++ {
		fp := "fp-A"
		if i%4 == 3 {
			fp = "fp-B"
		}
		r := ld.RecordToolCalls(fp)
		if r.Detected {
			t.Fatalf("unexpected early detection at iteration %d: %s", i, r.Reason)
		}
	}
	// 13th iteration (i=12): fp-A again -> 10 As in last 12 -> detected (Layer 1b)
	r := ld.RecordToolCalls("fp-A")
	if !r.Detected {
		t.Fatal("expected detection on alternating pattern when count reaches threshold")
	}
}

func TestLoopDetector_OutputRepeat(t *testing.T) {
	ld := NewLoopDetector()
	sameOutput := "this is the same output"
	for i := 0; i < 7; i++ {
		r := ld.RecordOutput(sameOutput)
		if r.Detected {
			t.Fatalf("unexpected output detection at %d", i)
		}
	}
	r := ld.RecordOutput(sameOutput)
	if !r.Detected {
		t.Fatal("expected output detection on 8th repeat (outThresh=8)")
	}
	if r.Layer != 2 {
		t.Fatalf("expected layer 2, got %d", r.Layer)
	}
}

func TestLoopDetector_DifferentOutput_NoTrigger(t *testing.T) {
	ld := NewLoopDetector()
	for i := 0; i < 100; i++ {
		r := ld.RecordOutput(strings.Repeat(string(rune('a'+i%26)), 10))
		if r.Detected {
			t.Fatalf("unexpected output detection: %s", r.Reason)
		}
	}
}

func TestLoopDetector_Reset(t *testing.T) {
	ld := NewLoopDetector()
	// Build up history
	for i := 0; i < 5; i++ {
		ld.RecordToolCalls("fp-abc")
	}
	ld.RecordOutput("same")

	// Reset
	ld.Reset()

	// After reset, previous counts should be gone
	for i := 0; i < 5; i++ {
		r := ld.RecordToolCalls("fp-abc")
		if r.Detected {
			t.Fatal("unexpected detection after reset")
		}
	}
}

func TestLoopDetector_EmptyFingerprint(t *testing.T) {
	ld := NewLoopDetector()
	for i := 0; i < 20; i++ {
		r := ld.RecordToolCalls("")
		if r.Detected {
			t.Fatal("empty fingerprint should never trigger detection")
		}
	}
}

func TestLoopDetector_EmptyOutput(t *testing.T) {
	ld := NewLoopDetector()
	for i := 0; i < 20; i++ {
		r := ld.RecordOutput("")
		if r.Detected {
			t.Fatal("empty output should never trigger detection")
		}
	}
}

func TestLoopDetector_ResetFingerprintHistory(t *testing.T) {
	ld := NewLoopDetector()
	// Build up 10 same fingerprints to trigger detection (toolOnlyThresh=10)
	for i := 0; i < 10; i++ {
		ld.RecordToolCalls("fp-abc")
	}

	// Reset fingerprint history
	ld.ResetFingerprintHistory()

	// After reset, should be able to start fresh
	for i := 0; i < 9; i++ {
		r := ld.RecordToolCalls("fp-abc")
		if r.Detected {
			t.Fatalf("unexpected detection after ResetFingerprintHistory at %d", i)
		}
	}
	// 10th call triggers detection again (L1b: 10/12, breakCount already 1 from first)
	r := ld.RecordToolCalls("fp-abc")
	if !r.Detected {
		t.Fatal("expected detection on 10th call after ResetFingerprintHistory")
	}
}

func TestLoopDetector_EscalationToHardStop(t *testing.T) {
	ld := NewLoopDetector()
	// First detection -> guidance mode (non-fatal, breakCount=1)
	// L1b fires at 10/12 (i=9)
	for i := 0; i < 10; i++ {
		r := ld.RecordToolCalls("fp-xyz")
		if i < 9 && r.Detected {
			t.Fatalf("unexpected early detection at %d", i)
		}
		if i == 9 {
			if !r.Detected || r.Fatal {
				t.Fatalf("first detection should be non-fatal, got fatal=%v", r.Fatal)
			}
		}
	}

	// Second -> fourth detection: non-fatal (breakCount 2-4 < maxBreaks=5)
	for breakCount := 2; breakCount <= 4; breakCount++ {
		r := ld.RecordToolCalls("fp-xyz")
		if !r.Detected {
			t.Fatalf("expected detection on breakCount %d", breakCount)
		}
		if r.Fatal {
			t.Fatalf("breakCount %d should be non-fatal (< maxBreaks=5)", breakCount)
		}
	}

	// Fifth detection -> fatal (breakCount=5 >= maxBreaks=5)
	r := ld.RecordToolCalls("fp-xyz")
	if !r.Detected {
		t.Fatal("expected fifth detection")
	}
	if !r.Fatal {
		t.Fatal("fifth consecutive detection should be fatal (breakCount=5 >= maxBreaks=5)")
	}
}

func TestLoopDetector_OutputWindowSliding(t *testing.T) {
	ld := NewLoopDetector()
	// Fill window with 50 unique outputs
	for i := 0; i < 50; i++ {
		ld.RecordOutput(strings.Repeat(string(rune('a'+i%26)), 10))
	}
	// Now push 9 identical outputs, each pushing one unique out
	same := "looping output"
	for i := 0; i < 7; i++ {
		r := ld.RecordOutput(same)
		if r.Detected {
			t.Fatalf("unexpected detection at %d", i)
		}
	}
	r := ld.RecordOutput(same)
	if !r.Detected {
		cnt := ld.outCounts[hashPrefix(same, 512)]
		t.Fatalf("expected detection at 8th (cnt=%d, window=%d)", cnt, ld.outWindow)
	}
}

func TestLoopDetector_L1bProgressDetection_DiverseOutput(t *testing.T) {
	ld := NewLoopDetector()
	// Simulate the false-positive scenario: same tool (powershell) called
	// 12 times with DIFFERENT commands (different fingerprints) but same
	// tool-only pattern (because first 20 chars are truncated).
	// E.g. "powershell:cd G:\github\cove\ag" is the tool-only pattern for
	// all of: "cd G:\github\cove\agent; git status", "cd G:\github\cove\agent; git checkout", etc.
	// Layer 1b would normally trigger (12/12 same tool pattern),
	// but progress detection should suppress because outputs are diverse.
	for i := 0; i < 12; i++ {
		// Different fingerprint each time (different full command)
		cmds := []string{
			"cd G:\\github\\cove\\agent; git status",
			"cd G:\\github\\cove\\agent; git checkout main",
			"cd G:\\github\\cove\\agent; git stash",
			"cd G:\\github\\cove\\agent; git diff --name-only",
			"cd G:\\github\\cove\\agent; git log --oneline -1",
			"cd G:\\github\\cove\\agent; git add --force test",
			"cd G:\\github\\cove\\agent; git commit -m test",
			"cd G:\\github\\cove\\agent; git stash pop",
			"cd G:\\github\\cove\\agent; git branch",
			"cd G:\\github\\cove\\agent; git remote -v",
			"cd G:\\github\\cove\\agent; git config --list",
			"cd G:\\github\\cove\\agent; git fetch --all",
		}
		fp := "powershell:" + cmds[i%len(cmds)]
		r := ld.RecordToolCalls(fp)
		if r.Detected {
			t.Fatalf("unexpected L1 detection at iteration %d (fp=%s): %s", i, fp, r.Reason)
		}
		// Each call produces a different output -> this is progress
		output := "different output " + string(rune('A'+i))
		r2 := ld.RecordOutput(output)
		if r2.Detected {
			t.Fatalf("unexpected L2 detection at iteration %d: %s", i, r2.Reason)
		}
	}
	// Verify that toolOnlyHistory was cleared (progress detection worked)
	if len(ld.toolOnlyHistory) > 0 {
		t.Fatalf("expected toolOnlyHistory to be cleared after diverse outputs, got %d entries: %v",
			len(ld.toolOnlyHistory), ld.toolOnlyHistory)
	}
	// Verify that toolOutputs tracking worked
	pattern := "powershell:cd G:\\github\\cove\\ag"
	outs := ld.toolOutputs[pattern]
	if len(outs) == 0 {
		t.Fatalf("expected output hashes tracked for pattern %s", pattern)
	}
	// Verify diversity
	uniq := make(map[string]bool)
	for _, h := range outs {
		uniq[h] = true
	}
	if len(uniq) < 2 {
		t.Fatal("expected at least 2 unique output hashes (progress)")
	}
}

func TestLoopDetector_L1bProgressDetection_SameOutput(t *testing.T) {
	ld := NewLoopDetector()
	// Same tool pattern with 2 ALTERNATING outputs.
	// 2 unique hashes < 4 threshold, so progress detection won't suppress L1b.
	// Layer 2 won't fire because outputs aren't identical.
	for i := 0; i < 9; i++ {
		cmds := []string{
			"cd G:\\github\\cove\\agent; git status",
			"cd G:\\github\\cove\\agent; git stash",
			"cd G:\\github\\cove\\agent; git checkout",
			"cd G:\\github\\cove\\agent; git branch",
		}
		fp := "powershell:" + cmds[i%len(cmds)]
		r := ld.RecordToolCalls(fp)
		if r.Detected {
			t.Fatalf("unexpected detection at %d", i)
		}
		// Alternate between 2 outputs (not diverse enough for progress)
		r2 := ld.RecordOutput("result variant " + string(rune('A'+i%2)))
		if r2.Detected {
			t.Fatalf("unexpected L2 detection at %d", i)
		}
	}
	// 10th call -> L1b should fire (10/12 same tool pattern, only 2 unique outputs < 4)
	r := ld.RecordToolCalls("powershell:cd G:\\github\\cove\\agent; git status")
	if !r.Detected {
		t.Fatal("expected detection on 10th repeat with same outputs (no progress)")
	}
	if r.Layer != 1 {
		t.Fatalf("expected Layer 1, got %d", r.Layer)
	}
}
