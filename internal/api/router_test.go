package api

import (
	"context"
	"strings"
	"testing"
)

func TestModelRouter_OverrideWins(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	mr.SetOverride("user-pick")
	d := mr.Route(context.Background(), "anything")
	if d.Model != "user-pick" || d.Source != "override" {
		t.Fatalf("override should win: got %+v", d)
	}
	mr.ClearOverride()
	if mr.Route(context.Background(), "hi").Source == "override" {
		t.Fatal("override not cleared")
	}
}

func TestModelRouter_ComplexityClassifier(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	if d := mr.Route(context.Background(), "请重构整个模块"); d.Model != "premium" {
		t.Fatalf("complex task should route to premium, got %+v", d)
	}
	if d := mr.Route(context.Background(), "hi"); d.Model != "fast" {
		t.Fatalf("simple task should route to fast, got %+v", d)
	}
}

// Regression: after /model or /provider changes the active model, the router
// must track it instead of routing to the construction-time model.
func TestModelRouter_SetModelsSync(t *testing.T) {
	mr := NewModelRouter("old-premium", "old-fast")
	mr.SetModels("new-premium", "new-fast")

	if d := mr.Route(context.Background(), "重构架构"); d.Model != "new-premium" {
		t.Fatalf("complex task should use updated premium model, got %+v", d)
	}
	if d := mr.Route(context.Background(), "hi"); d.Model != "new-fast" {
		t.Fatalf("simple task should use updated fast model, got %+v", d)
	}

	// Empty args leave existing models untouched.
	mr.SetModels("", "")
	if mr.defaultModel != "new-premium" || mr.fastModel != "new-fast" {
		t.Fatalf("empty SetModels should be a no-op, got %q/%q", mr.defaultModel, mr.fastModel)
	}
}

// TestModelRouter_HardLengthCeiling: an extremely long message forces the
// premium model outright, regardless of other signals — this is the one
// case that stays a hard rule rather than a weighted contribution, since a
// message this long genuinely does need deeper context handling.
func TestModelRouter_HardLengthCeiling(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	longMsg := make([]byte, hardLengthCeiling+1)
	for i := range longMsg {
		longMsg[i] = 'a'
	}
	d := mr.Route(context.Background(), string(longMsg))
	if d.Model != "premium" {
		t.Fatalf("message over hard length ceiling should force premium, got %+v", d)
	}
}

// TestModelRouter_ModerateLengthAloneStaysFast: this is an intentional
// behavior change from the old hard 500-char cutoff — a moderately long
// message with no other signal should no longer force premium by itself,
// since length is now one weighted signal among several rather than an
// absolute rule.
func TestModelRouter_ModerateLengthAloneStaysFast(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	msg := strings.Repeat("please rename this variable ", 20) // ~580 chars, no keywords
	d := mr.Route(context.Background(), msg)
	if d.Model != "fast" {
		t.Fatalf("moderate-length message with no other signal should stay on fast model, got %+v", d)
	}
}

type fakeBudgetSignal struct{ ratio float64 }

func (f fakeBudgetSignal) RemainingBudgetRatio() float64 { return f.ratio }

type fakeFailureRateSignal struct{ rate float64 }

func (f fakeFailureRateSignal) RecentFastModelFailureRate() float64 { return f.rate }

// TestModelRouter_TightBudgetSuppressesUpgrade: a borderline score (from
// file-scope + length signals, not strong enough alone) should be pushed
// further away from the premium threshold when budget is nearly exhausted.
func TestModelRouter_TightBudgetSuppressesUpgrade(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	msg := "please update main.go and handler.go and util.go together"

	// With plenty of budget, the file-mention signal alone should not be
	// enough to cross the threshold (this also documents the baseline).
	mr.SetBudgetSignal(fakeBudgetSignal{ratio: 1.0})
	baseline := mr.Route(context.Background(), msg)

	// With budget nearly exhausted, the score can only go down, never up —
	// so if baseline was already fast, tight budget must also be fast.
	mr.SetBudgetSignal(fakeBudgetSignal{ratio: 0.0})
	tight := mr.Route(context.Background(), msg)

	if baseline.Model == "fast" && tight.Model != "fast" {
		t.Fatalf("tight budget should never push a borderline-fast decision to premium: baseline=%+v tight=%+v", baseline, tight)
	}
}

// TestModelRouter_HighFailureRateCanTipToPremium: a high recent failure
// rate for the fast model is a real (if weak) signal that this session
// needs the premium model more readily. Constructed so length(~0.17) +
// file-scope(0.15) alone sit just under the 0.40 threshold (~0.32), and a
// maxed-out failure rate (+0.10) is exactly what tips it over — this
// exercises the signal actually changing the outcome, not just being
// present without effect.
func TestModelRouter_HighFailureRateCanTipToPremium(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	msg := strings.Repeat("lorem ", 140) + "main.go handler.go util.go"

	mr.SetFailureRateSignal(fakeFailureRateSignal{rate: 0})
	withoutHistory := mr.Route(context.Background(), msg)
	if withoutHistory.Model != "fast" {
		t.Fatalf("expected borderline score without failure history to stay fast, got %+v", withoutHistory)
	}

	mr.SetFailureRateSignal(fakeFailureRateSignal{rate: 1.0})
	withHistory := mr.Route(context.Background(), msg)
	if withHistory.Model != "premium" {
		t.Fatalf("expected high failure rate to tip the borderline score to premium, got %+v", withHistory)
	}
}
