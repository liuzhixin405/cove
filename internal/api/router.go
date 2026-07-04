package api

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// RoutingDecision is the result of model routing.
type RoutingDecision struct {
	Model  string `json:"model"`
	Source string `json:"source"` // classifier, override, fallback, default
	Reason string `json:"reason"`
}

// BudgetSignal lets the scoring strategy factor remaining budget into its
// decision without ModelRouter importing internal/cost directly (keeping
// the api package's dependency graph shallow). Engine wires this in via
// SetBudgetSignal; if never set, budget simply doesn't affect scoring.
type BudgetSignal interface {
	// RemainingBudgetRatio returns remaining budget / max budget, in [0,1].
	// Implementations with no configured budget (unlimited) should return 1.
	RemainingBudgetRatio() float64
}

// FailureRateSignal lets the scoring strategy factor in how often recent
// fast-model-routed turns have needed to give up (verification failures,
// tool-failure circuit breaker) rather than completing cleanly. If never
// set via SetFailureRateSignal, failure rate simply doesn't affect scoring.
type FailureRateSignal interface {
	// RecentFastModelFailureRate returns the fraction (0..1) of recent
	// fast-model-routed turns that ended in a give-up/failure state.
	RecentFastModelFailureRate() float64
}

// ModelRouter selects the best model for a given user message using
// a chain of routing strategies. The first strategy that returns
// a non-nil decision wins.
type ModelRouter struct {
	strategies   []RoutingStrategy
	defaultModel string // 高级模型，用于复杂任务（如 deepseek-v4-pro）
	fastModel    string // 快速模型，用于简单任务（如 deepseek-v4-flash）
	override     string // user-specified override (e.g. /model gpt-4o)
	budget       BudgetSignal
	failureRate  FailureRateSignal
}

// RoutingStrategy evaluates a user message and decides whether to route.
type RoutingStrategy interface {
	Route(ctx context.Context, userMessage string, defaultModel string) *RoutingDecision
	Name() string
}

// NewModelRouter creates a router with the standard strategy chain:
// override → scoring classifier → default.
func NewModelRouter(defaultModel, fastModel string) *ModelRouter {
	mr := &ModelRouter{defaultModel: defaultModel, fastModel: fastModel}
	mr.strategies = []RoutingStrategy{
		&overrideStrategy{router: mr},
		&complexityClassifier{router: mr},
	}
	return mr
}

// SetModels updates the default (premium) and fast models. Call this whenever
// the active model/provider changes (e.g. /model, /provider) so routing tracks
// the current configuration instead of the construction-time values. Passing an
// empty fastModel leaves the existing fast model unchanged.
func (mr *ModelRouter) SetModels(defaultModel, fastModel string) {
	if defaultModel != "" {
		mr.defaultModel = defaultModel
	}
	if fastModel != "" {
		mr.fastModel = fastModel
	}
}

// SetOverride sets a user-specified model override (e.g. from /model command).
func (mr *ModelRouter) SetOverride(model string) { mr.override = model }

// ClearOverride removes the user override.
func (mr *ModelRouter) ClearOverride() { mr.override = "" }

// SetBudgetSignal wires in a source of remaining-budget information for the
// scoring strategy. Optional — nil (the default) means budget pressure does
// not affect routing.
func (mr *ModelRouter) SetBudgetSignal(b BudgetSignal) { mr.budget = b }

// SetFailureRateSignal wires in a source of recent fast-model failure-rate
// information for the scoring strategy. Optional — nil (the default) means
// failure history does not affect routing.
func (mr *ModelRouter) SetFailureRateSignal(f FailureRateSignal) { mr.failureRate = f }

// Route evaluates the full strategy chain.
func (mr *ModelRouter) Route(ctx context.Context, userMessage string) *RoutingDecision {
	for _, s := range mr.strategies {
		if decision := s.Route(ctx, userMessage, mr.defaultModel); decision != nil {
			return decision
		}
	}
	return &RoutingDecision{Model: mr.defaultModel, Source: "default", Reason: "no strategy matched"}
}

// ──── Strategies ────

// overrideStrategy checks for user-specified model override.
type overrideStrategy struct {
	router *ModelRouter
}

func (s *overrideStrategy) Name() string { return "override" }

func (s *overrideStrategy) Route(_ context.Context, _ string, _ string) *RoutingDecision {
	if s.router.override != "" {
		return &RoutingDecision{
			Model:  s.router.override,
			Source: "override",
			Reason: "user-specified model",
		}
	}
	return nil
}

// ──── Multi-factor scoring classifier ────
//
// This replaces the old "first matching rule wins" classifier with a
// weighted score combining several independent, individually-weak signals.
// This is the concrete, minimal implementation of the "路由升级为多特征打分"
// item from docs/核心优化项清单.md's EDCL proposal: no full evidence ledger
// yet, just multiple factors instead of one keyword match, with the exact
// contribution of each factor always written into RoutingDecision.Reason so
// a decision can be understood (and, later, audited) after the fact.
//
// Weights are deliberately set so that a single strong signal (an explicit
// complexity keyword) is, by itself, still enough to cross the premium
// threshold — matching the previous behavior for that case — while weaker
// signals only tip the balance in combination with each other.
const (
	weightComplexKeyword = 0.45
	weightMessageLength  = 0.20
	weightFileScope      = 0.15
	weightFailureRate    = 0.10
	weightBudget         = 0.10 // subtracted when budget is tight, not added

	// scoreThreshold is the minimum combined score to route to the premium
	// (default) model instead of the fast model.
	scoreThreshold = 0.40

	// hardLengthCeiling: regardless of other signals, a message this long
	// always needs the premium model's deeper context handling.
	hardLengthCeiling = 2000
)

type complexityClassifier struct {
	router *ModelRouter
}

func (c *complexityClassifier) Name() string { return "classifier" }

var complexKeywords = []string{
	"refactor", "architecture", "design", "migrate", "rewrite",
	"debug", "optimize", "performance", "security audit",
	"重构", "架构", "设计", "迁移", "重写",
}

// filePathPattern is a deliberately loose heuristic for "the user named
// specific files/paths in this message" — used as a cheap, best-effort
// proxy for change scope before the model has actually looked at anything.
// It is not meant to be a precise path parser.
var filePathPattern = regexp.MustCompile(`[\w./\\-]+\.(go|py|js|ts|tsx|jsx|java|rs|c|cpp|h|hpp|rb|php|json|yaml|yml|md|sql)\b`)

func (c *complexityClassifier) Route(_ context.Context, userMessage string, _ string) *RoutingDecision {
	msg := strings.ToLower(userMessage)

	var reasons []string
	score := 0.0

	// Signal 1: explicit complexity keyword (binary).
	matchedKeyword := ""
	for _, kw := range complexKeywords {
		if strings.Contains(msg, kw) {
			matchedKeyword = kw
			break
		}
	}
	if matchedKeyword != "" {
		score += weightComplexKeyword
		reasons = append(reasons, fmt.Sprintf("keyword(%q)=+%.2f", matchedKeyword, weightComplexKeyword))
	}

	// Signal 2: message length, graduated rather than a hard cutoff (a
	// message just over the old 500-char cutoff no longer forces premium
	// by itself; a genuinely long one still contributes strongly).
	length := len(userMessage)
	if length >= hardLengthCeiling {
		return &RoutingDecision{
			Model:  c.router.defaultModel,
			Source: "classifier",
			Reason: fmt.Sprintf("message length %d >= hard ceiling %d, forcing premium model", length, hardLengthCeiling),
		}
	}
	lengthScore := float64(length) / 1000.0
	if lengthScore > 1 {
		lengthScore = 1
	}
	if lengthScore > 0 {
		contrib := lengthScore * weightMessageLength
		score += contrib
		reasons = append(reasons, fmt.Sprintf("length(%d)=+%.2f", length, contrib))
	}

	// Signal 3: explicit file/path mentions, as a cheap proxy for change
	// scope (we can't know the real diff size before the model acts).
	if matches := filePathPattern.FindAllString(userMessage, -1); len(matches) > 0 {
		fileScore := float64(len(matches)) / 3.0
		if fileScore > 1 {
			fileScore = 1
		}
		contrib := fileScore * weightFileScope
		score += contrib
		reasons = append(reasons, fmt.Sprintf("file_mentions(%d)=+%.2f", len(matches), contrib))
	}

	// Signal 4: recent fast-model failure rate on this project/session.
	if c.router.failureRate != nil {
		rate := c.router.failureRate.RecentFastModelFailureRate()
		if rate > 0 {
			contrib := rate * weightFailureRate
			score += contrib
			reasons = append(reasons, fmt.Sprintf("recent_failure_rate(%.2f)=+%.2f", rate, contrib))
		}
	}

	// Signal 5: budget pressure. Tight budget makes the router *less* eager
	// to upgrade — it subtracts from the score rather than adding to it.
	if c.router.budget != nil {
		remaining := c.router.budget.RemainingBudgetRatio()
		if remaining < 1 {
			penalty := (1 - remaining) * weightBudget
			score -= penalty
			reasons = append(reasons, fmt.Sprintf("budget_remaining(%.2f)=-%.2f", remaining, penalty))
		}
	}

	reasonStr := strings.Join(reasons, ", ")
	if reasonStr == "" {
		reasonStr = "no signals present"
	}

	if score >= scoreThreshold {
		return &RoutingDecision{
			Model:  c.router.defaultModel,
			Source: "classifier",
			Reason: fmt.Sprintf("score=%.2f >= %.2f [%s] -> premium model", score, scoreThreshold, reasonStr),
		}
	}

	if c.router.fastModel != "" {
		return &RoutingDecision{
			Model:  c.router.fastModel,
			Source: "classifier",
			Reason: fmt.Sprintf("score=%.2f < %.2f [%s] -> fast model", score, scoreThreshold, reasonStr),
		}
	}

	return nil // no fast model configured, fall back to default
}
