package engine

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/token"
)

func TestContextBudgeter_EmptyContentIgnored(t *testing.T) {
	b := newContextBudgeter(1000)
	b.add(layerRelevant, "")
	b.add(layerRelevant, "   \n\t ")
	if got := b.Render(); got != "" {
		t.Fatalf("expected empty render for whitespace-only sections, got %q", got)
	}
}

func TestContextBudgeter_HigherPriorityLayerRenderedFirst(t *testing.T) {
	b := newContextBudgeter(1000)
	// Add overflow first, then relevant, to prove ordering is by layer,
	// not insertion order.
	b.add(layerOverflow, "OVERFLOW_CONTENT")
	b.add(layerRelevant, "RELEVANT_CONTENT")
	b.add(layerOnDemand, "ONDEMAND_CONTENT")

	got := b.Render()
	relevantIdx := strings.Index(got, "RELEVANT_CONTENT")
	onDemandIdx := strings.Index(got, "ONDEMAND_CONTENT")
	overflowIdx := strings.Index(got, "OVERFLOW_CONTENT")
	if relevantIdx == -1 || onDemandIdx == -1 || overflowIdx == -1 {
		t.Fatalf("expected all three sections present when budget is generous, got %q", got)
	}
	if !(relevantIdx < onDemandIdx && onDemandIdx < overflowIdx) {
		t.Fatalf("expected layer order relevant < on-demand < overflow, got %q", got)
	}
}

func TestContextBudgeter_LowerPriorityDroppedWhenBudgetExhausted(t *testing.T) {
	relevant := strings.Repeat("x", 300) // ~100 tokens per token.Estimate (3 ascii bytes/token)
	overflow := "SHOULD_NOT_APPEAR"

	// Budget only large enough for the relevant section, with nothing left
	// for overflow.
	budget := token.Estimate(relevant)
	b := newContextBudgeter(budget)
	b.add(layerRelevant, relevant)
	b.add(layerOverflow, overflow)

	got := b.Render()
	if strings.Contains(got, overflow) {
		t.Fatalf("expected overflow layer to be dropped once budget is exhausted, got %q", got)
	}
	if !strings.Contains(got, relevant) {
		t.Fatalf("expected relevant layer to be fully included, got %q", got)
	}
}

func TestContextBudgeter_PartialSectionTruncatedNotDropped(t *testing.T) {
	// A single section larger than the whole budget should be truncated
	// to fit, not dropped outright — some of the highest-priority content
	// is better than none.
	big := strings.Repeat("word ", 2000) // far more than 50 tokens
	b := newContextBudgeter(50)
	b.add(layerRelevant, big)

	got := b.Render()
	if got == "" {
		t.Fatal("expected truncated content, got empty string")
	}
	if got == big {
		t.Fatal("expected content to be truncated, got the full untruncated string")
	}
	if token.Estimate(got) > 60 { // truncation lands near the boundary, not exact
		t.Fatalf("expected truncated content roughly within budget, got estimated %d tokens", token.Estimate(got))
	}
}

func TestContextBudgeter_NonPositiveBudgetFallsBackToDefault(t *testing.T) {
	b := newContextBudgeter(0)
	if b.budget <= 0 {
		t.Fatalf("expected a positive fallback budget, got %d", b.budget)
	}
	b2 := newContextBudgeter(-5)
	if b2.budget <= 0 {
		t.Fatalf("expected a positive fallback budget for negative input, got %d", b2.budget)
	}
}
