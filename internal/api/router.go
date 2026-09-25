package api

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
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
	strategies []RoutingStrategy

	// mu guards every mutable field below. The setters are driven by the UI
	// goroutine (/model, /provider) while Route runs on the engine goroutine,
	// and background turn-end work reads them too, so unsynchronized access is
	// a genuine race. Read them through the accessors, never directly — the
	// strategies live in this file and do exactly that.
	mu           sync.RWMutex
	defaultModel string // 高级模型，用于复杂任务（如 deepseek-v4-pro）
	fastModel    string // 快速模型，用于简单任务（如 deepseek-v4-flash）
	override     string // user-specified override (e.g. /model gpt-4o)
	budget       BudgetSignal
	failureRate  FailureRateSignal
	lastRouted   string // model chosen by the last Route
}

// DefaultModel returns the configured premium model.
func (mr *ModelRouter) DefaultModel() string {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return mr.defaultModel
}

// FastModel returns the configured fast model ("" when none).
func (mr *ModelRouter) FastModel() string {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return mr.fastModel
}

// Override returns the user-specified model override ("" when none).
func (mr *ModelRouter) Override() string {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return mr.override
}

func (mr *ModelRouter) budgetSignal() BudgetSignal {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return mr.budget
}

func (mr *ModelRouter) failureRateSignal() FailureRateSignal {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return mr.failureRate
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
	mr.mu.Lock()
	defer mr.mu.Unlock()
	if defaultModel != "" {
		mr.defaultModel = defaultModel
	}
	if fastModel != "" {
		mr.fastModel = fastModel
	}
}

// SetOverride sets a user-specified model override (e.g. from /model command).
func (mr *ModelRouter) SetOverride(model string) {
	mr.mu.Lock()
	defer mr.mu.Unlock()
	mr.override = model
}

// ClearOverride removes the user override.
func (mr *ModelRouter) ClearOverride() {
	mr.mu.Lock()
	defer mr.mu.Unlock()
	mr.override = ""
}

// SetBudgetSignal wires in a source of remaining-budget information for the
// scoring strategy. Optional — nil (the default) means budget pressure does
// not affect routing.
func (mr *ModelRouter) SetBudgetSignal(b BudgetSignal) {
	mr.mu.Lock()
	defer mr.mu.Unlock()
	mr.budget = b
}

// SetFailureRateSignal wires in a source of recent fast-model failure-rate
// information for the scoring strategy. Optional — nil (the default) means
// failure history does not affect routing.
func (mr *ModelRouter) SetFailureRateSignal(f FailureRateSignal) {
	mr.mu.Lock()
	defer mr.mu.Unlock()
	mr.failureRate = f
}

// Route evaluates the full strategy chain.
func (mr *ModelRouter) Route(ctx context.Context, userMessage string) *RoutingDecision {
	// Snapshot once so every strategy in this chain sees the same default,
	// even if SetModels lands mid-evaluation.
	defaultModel := mr.DefaultModel()
	decision := &RoutingDecision{Model: defaultModel, Source: "default", Reason: "no strategy matched"}
	for _, s := range mr.strategies {
		if d := s.Route(ctx, userMessage, defaultModel); d != nil {
			decision = d
			break
		}
	}
	mr.mu.Lock()
	mr.lastRouted = decision.Model
	mr.mu.Unlock()
	return decision
}

// RoutedModelLabel is the model the last Route chose, for the status line at
// the start of a turn ("模型：<label>"). It is empty when routing cannot pick
// between two models (no fast model, or fast == main) or nothing was routed
// yet, so the line only appears when it tells the user something.
func (mr *ModelRouter) RoutedModelLabel() string {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	if mr.fastModel == "" || mr.fastModel == mr.defaultModel {
		return ""
	}
	return mr.lastRouted
}

// ──── Strategies ────

// overrideStrategy checks for user-specified model override.
type overrideStrategy struct {
	router *ModelRouter
}

func (s *overrideStrategy) Name() string { return "override" }

func (s *overrideStrategy) Route(_ context.Context, _ string, _ string) *RoutingDecision {
	if override := s.router.Override(); override != "" {
		return &RoutingDecision{
			Model:  override,
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
	// 0.40 was unreachable without a keyword (length 0.20 + files 0.15).
	scoreThreshold = 0.35

	// hardLengthCeiling: regardless of other signals, a message this long
	// (in characters) always needs the premium model's deeper context
	// handling.
	hardLengthCeiling = 2000
)

type complexityClassifier struct {
	router *ModelRouter
}

func (c *complexityClassifier) Name() string { return "classifier" }

// Each English keyword has a Chinese counterpart; the list used to stop at
// 重写, so "调试/优化/性能/安全审计" went to the fast model while the same
// request in English went to the premium one.
var complexKeywords = []string{
	"refactor", "architecture", "design", "migrate", "rewrite",
	"debug", "optimize", "performance", "security audit",
	"重构", "架构", "设计", "迁移", "重写",
	"调试", "优化", "性能", "安全审计",
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
	// Characters, not bytes: a Chinese character is three bytes, so byte
	// length sent short Chinese messages to the premium model.
	length := utf8.RuneCountInString(userMessage)
	if length >= hardLengthCeiling {
		return &RoutingDecision{
			Model:  c.router.DefaultModel(),
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
	if fr := c.router.failureRateSignal(); fr != nil {
		rate := fr.RecentFastModelFailureRate()
		if rate > 0 {
			contrib := rate * weightFailureRate
			score += contrib
			reasons = append(reasons, fmt.Sprintf("recent_failure_rate(%.2f)=+%.2f", rate, contrib))
		}
	}

	// Signal 5: budget pressure. Tight budget makes the router *less* eager
	// to upgrade — it subtracts from the score rather than adding to it.
	if b := c.router.budgetSignal(); b != nil {
		remaining := b.RemainingBudgetRatio()
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
			Model:  c.router.DefaultModel(),
			Source: "classifier",
			Reason: fmt.Sprintf("score=%.2f >= %.2f [%s] -> premium model", score, scoreThreshold, reasonStr),
		}
	}

	if fast := c.router.FastModel(); fast != "" {
		return &RoutingDecision{
			Model:  fast,
			Source: "classifier",
			Reason: fmt.Sprintf("score=%.2f < %.2f [%s] -> fast model", score, scoreThreshold, reasonStr),
		}
	}

	return nil // no fast model configured, fall back to default
}
