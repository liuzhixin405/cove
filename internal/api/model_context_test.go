package api

import "testing"

func TestContextWindowForModel_KnownFamilies(t *testing.T) {
	cases := map[string]int{
		"claude-sonnet-4-20250514": 200000,
		"deepseek-v4-flash":        128000,
		"deepseek-chat":            64000,
		"gpt-4o-mini":              128000,
		"qwen-turbo":               32000,
		"totally-unknown-model-x":  defaultContextWindow,
	}
	for model, want := range cases {
		if got := ContextWindowForModel(model); got != want {
			t.Errorf("ContextWindowForModel(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestUtilizationRatioForModel_FastVsPremium(t *testing.T) {
	if r := UtilizationRatioForModel("deepseek-v4-flash"); r != 0.65 {
		t.Errorf("expected fast model ratio 0.65, got %v", r)
	}
	if r := UtilizationRatioForModel("claude-sonnet-4-20250514"); r != 0.85 {
		t.Errorf("expected premium model ratio 0.85, got %v", r)
	}
}

func TestEffectiveCompactionBudget_FastModelLowerThanPremium(t *testing.T) {
	fast := EffectiveCompactionBudget("deepseek-v4-flash")   // 128000 * 0.65 * 0.7
	premium := EffectiveCompactionBudget("deepseek-v4-pro")  // 128000 * 0.85 * 0.7
	if fast >= premium {
		t.Fatalf("expected fast-model budget (%d) to be lower than premium-model budget (%d) for the same context window", fast, premium)
	}
}

func TestStaticContextBudget_FloorForSmallWindow(t *testing.T) {
	// qwen-turbo: window=32000, ratio=0.85 (not a fast-budget name) ->
	// 32000*0.85*0.3 = 8160, comfortably above the 2000 floor, so this
	// locks in the arithmetic rather than the floor branch.
	want := int(float64(32000) * 0.85 * 0.3)
	if got := StaticContextBudget("qwen-turbo"); got != want {
		t.Fatalf("StaticContextBudget(qwen-turbo) = %d, want %d", got, want)
	}
}

func TestStaticContextBudget_CeilingForHugeWindow(t *testing.T) {
	// qwen-long: window=1,000,000, ratio=0.85 -> raw = 255000, far above
	// the 24000 ceiling, so the ceiling clamp must kick in.
	got := StaticContextBudget("qwen-long")
	if got != 24000 {
		t.Fatalf("StaticContextBudget(qwen-long) = %d, want ceiling 24000", got)
	}
}

func TestStaticContextBudget_LowerThanCompactionBudgetForSameModel(t *testing.T) {
	// Static context and compaction budget are complementary shares (0.3
	// vs 0.7) of the same effective window, so for any given model the
	// static share must be smaller.
	model := "deepseek-v4-pro" // window=128000 (matches "deepseek-v4"), ratio=0.85 (not fast-budget)
	static := StaticContextBudget(model)
	compaction := EffectiveCompactionBudget(model)
	if static >= compaction {
		t.Fatalf("expected static budget (%d) < compaction budget (%d) for %q", static, compaction, model)
	}
}

func TestEffectiveCompactionBudget_MatchesFormula(t *testing.T) {
	// docs/中等模型平替优化建议.md §2.3: budget = window * utilization * 0.7.
	// With today's smallest configured window (32000) and lowest ratio
	// (0.65), the floor (4000) is never actually reached — this test locks
	// in the arithmetic itself rather than the (currently unreachable)
	// floor branch, which exists purely as a defensive guard against future
	// changes to the window/ratio tables.
	model := "qwen-turbo" // window=32000 (default bucket), fast=false -> ratio=0.85
	want := int(float64(32000) * 0.85 * 0.7)
	if got := EffectiveCompactionBudget(model); got != want {
		t.Fatalf("EffectiveCompactionBudget(%q) = %d, want %d", model, got, want)
	}
}
