package api

import "testing"

func TestContextWindowForModel_KnownFamilies(t *testing.T) {
	cases := map[string]int{
		"claude-sonnet-4-20250514": 200000,
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

// DeepSeek V4 models have a 1M-token context (api-docs.deepseek.com, models &
// pricing). Assuming 128K made Cove compact at ~76K tokens — and the current
// flash name, deepseek-flash, matched no pattern at all and fell to 32K.
// Current Claude models (Opus 4.6+, Sonnet 4.6+, Fable) are 1M as well.
func TestContextWindowForModel_OneMillionTokenModels(t *testing.T) {
	cases := map[string]int{
		"deepseek-v4-pro":   1000000,
		"deepseek-flash":    1000000,
		"deepseek-v4-flash": 1000000, // legacy name, routed to V4.1-Flash
		"claude-opus-5":     1000000,
		"claude-opus-4-6":   1000000,
		"claude-sonnet-4-6": 1000000,
		"claude-sonnet-5":   1000000,
		"claude-fable-5-1":  1000000,
		"claude-haiku-4-5":  200000,
	}
	for model, want := range cases {
		if got := ContextWindowForModel(model); got != want {
			t.Errorf("ContextWindowForModel(%q) = %d, want %d", model, got, want)
		}
	}
}

// A model named "flash" or "mini" is not assumed to use its context less
// well: current fast tiers are fully capable, and the discount only made Cove
// throw away history earlier for them.
func TestFastNamedModelsGetTheSameCompactionBudget(t *testing.T) {
	if fast, pro := EffectiveCompactionBudget("deepseek-flash"), EffectiveCompactionBudget("deepseek-v4-pro"); fast != pro {
		t.Fatalf("flash budget %d != pro budget %d for the same 1M window", fast, pro)
	}
}

// With a 1M window, a session carrying a few hundred thousand tokens of
// history is nowhere near full and must not be compacted.
func TestOneMillionWindowDoesNotCompactMidSizedSessions(t *testing.T) {
	if b := EffectiveCompactionBudget("deepseek-v4-pro"); b < 500000 {
		t.Fatalf("compaction budget for deepseek-v4-pro = %d; a 300K-token session would be compacted", b)
	}
}

func TestStaticContextBudget_FloorForSmallWindow(t *testing.T) {
	// qwen-turbo: window=32000, ratio=0.85 ->
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
	// Static context is a 0.3 share of the same effective window the
	// compaction budget spans, so for any given model it must be smaller.
	model := "deepseek-v4-pro"
	static := StaticContextBudget(model)
	compaction := EffectiveCompactionBudget(model)
	if static >= compaction {
		t.Fatalf("expected static budget (%d) < compaction budget (%d) for %q", static, compaction, model)
	}
}

func TestEffectiveCompactionBudget_MatchesFormula(t *testing.T) {
	// budget = window * utilization. The former extra × 0.7 reserve for the
	// system prompt and tools is gone: the engine's count now includes them.
	model := "qwen-turbo" // window=32000 (default bucket), ratio=0.85
	want := int(float64(32000) * 0.85)
	if got := EffectiveCompactionBudget(model); got != want {
		t.Fatalf("EffectiveCompactionBudget(%q) = %d, want %d", model, got, want)
	}
}
