package api

import "strings"

// contextWindowPattern maps a case-insensitive substring of a model name to
// its approximate context window size, in tokens. These are deliberately
// conservative approximations — a heuristic for sizing Cove's own
// compression threshold, not an authoritative API limit table, and never
// sent to the provider as a hard limit. Order matters: more specific
// patterns are listed before broader ones they'd otherwise be shadowed by.
type contextWindowPattern struct {
	pattern string
	window  int
}

var contextWindowPatterns = []contextWindowPattern{
	{"claude-opus", 200000},
	{"claude-sonnet", 200000},
	{"claude-haiku", 200000},
	{"claude-3", 200000},
	{"gpt-4o", 128000},
	{"gpt-4-turbo", 128000},
	{"o1", 128000},
	{"o3", 128000},
	{"o4", 128000},
	{"deepseek-v4", 128000},
	{"deepseek-chat", 64000},
	{"deepseek-reasoner", 64000},
	{"glm-4", 128000},
	{"glm", 128000},
	{"kimi", 128000},
	{"moonshot", 128000},
	{"qwen-long", 1000000},
	{"qwen", 32000},
	{"doubao", 32000},
	{"gemini-1.5", 1000000},
	{"gemini", 128000},
}

// defaultContextWindow is used for model names that don't match any known
// pattern — deliberately conservative so an unrecognized model compacts
// history sooner rather than risking an over-budget request.
const defaultContextWindow = 32000

// ContextWindowForModel returns an approximate context window size (in
// tokens) for the given model name, based on substring matching against
// known model families.
func ContextWindowForModel(model string) int {
	lower := strings.ToLower(model)
	for _, p := range contextWindowPatterns {
		if strings.Contains(lower, p.pattern) {
			return p.window
		}
	}
	return defaultContextWindow
}

// fastBudgetIndicators mirrors internal/engine's isFastModelName check. Kept
// as a small local duplicate (rather than an internal/engine ->
// internal/api dependency, which would invert the project's existing
// layering) since it's a one-line substring check.
var fastBudgetIndicators = []string{"flash", "mini", "lite", "tiny", "fast", "haiku", "nano"}

func isFastBudgetModel(model string) bool {
	lower := strings.ToLower(model)
	for _, ind := range fastBudgetIndicators {
		if strings.Contains(lower, ind) {
			return true
		}
	}
	return false
}

// UtilizationRatioForModel returns how much of a model's raw context window
// Cove should assume can actually be put to effective use before quality
// degrades on long-context tasks. Mid-tier/fast models are assumed to make
// less effective use of a long context than top-tier models — a
// deliberately conservative, coarse heuristic, not a measured benchmark.
func UtilizationRatioForModel(model string) float64 {
	if isFastBudgetModel(model) {
		return 0.65
	}
	return 0.85
}

// EffectiveCompactionBudget returns the token count at which Cove should
// start compacting conversation history for the given model: its
// (approximate) context window, scaled by an assumed effective-utilization
// ratio, with a further 30% safety buffer reserved for the system prompt,
// tool definitions, repo map, and the model's own response — matching the
// "安全预算 = model_context_limit × utilization_rate × 0.7" formula from
// docs/中等模型平替优化建议.md §2.3.
func EffectiveCompactionBudget(model string) int {
	window := ContextWindowForModel(model)
	ratio := UtilizationRatioForModel(model)
	budget := int(float64(window) * ratio * 0.7)
	const floor = 4000 // never compact so aggressively useful history can't fit at all
	if budget < floor {
		budget = floor
	}
	return budget
}

// StaticContextBudget returns the token budget Cove allocates to
// per-turn "static" system-prompt content that isn't the running
// conversation — matched skill prompts, retrieved memories, the repo
// map, and the project file tree. It's the complement of
// EffectiveCompactionBudget's 0.7 factor: that function reserves 30% of
// the effective window for exactly this content, so this returns that
// same 30% share, clamped to a sane floor/ceiling so extreme-context
// models (e.g. qwen-long's 1M-token window) don't get an unbounded
// allowance that would just get shipped to the provider unexamined.
//
// See internal/engine/context_budget.go, which spends this budget across
// priority layers (relevant > on-demand > overflow) instead of letting
// every section grow without bound and hoping the combined total happens
// to fit — the "Context分层预算器" item in
// docs/中等模型平替优化建议.md.
func StaticContextBudget(model string) int {
	window := ContextWindowForModel(model)
	ratio := UtilizationRatioForModel(model)
	budget := int(float64(window) * ratio * 0.3)
	const floor = 2000
	const ceiling = 24000
	if budget < floor {
		budget = floor
	}
	if budget > ceiling {
		budget = ceiling
	}
	return budget
}
