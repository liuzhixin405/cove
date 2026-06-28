package api

import (
	"context"
	"strings"
)

// RoutingDecision is the result of model routing.
type RoutingDecision struct {
	Model  string `json:"model"`
	Source string `json:"source"` // classifier, override, fallback, default
	Reason string `json:"reason"`
}

// ModelRouter selects the best model for a given user message using
// a chain of routing strategies. The first strategy that returns
// a non-nil decision wins.
type ModelRouter struct {
	strategies   []RoutingStrategy
	defaultModel string // 高级模型，用于复杂任务（如 deepseek-v4-pro）
	fastModel    string // 快速模型，用于简单任务（如 deepseek-v4-flash）
	override     string // user-specified override (e.g. /model gpt-4o)
}

// RoutingStrategy evaluates a user message and decides whether to route.
type RoutingStrategy interface {
	Route(ctx context.Context, userMessage string, defaultModel string) *RoutingDecision
	Name() string
}

// NewModelRouter creates a router with the standard strategy chain:
// override → fallback → complexity classifier → default.
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

// Override returns the current user override, if any.
func (mr *ModelRouter) Override() string { return mr.override }

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

// complexityClassifier evaluates message complexity to select cheap vs premium models.
type complexityClassifier struct {
	router *ModelRouter
}

func (c *complexityClassifier) Name() string { return "classifier" }

func (c *complexityClassifier) Route(_ context.Context, userMessage string, _ string) *RoutingDecision {
	msg := strings.ToLower(userMessage)
	length := len(userMessage)

	// Complex tasks → premium model (defaultModel)
	complexKeywords := []string{
		"refactor", "architecture", "design", "migrate", "rewrite",
		"debug", "optimize", "performance", "security audit",
		"重构", "架构", "设计", "迁移", "重写",
	}
	for _, kw := range complexKeywords {
		if strings.Contains(msg, kw) {
			return &RoutingDecision{
				Model:  c.router.defaultModel,
				Source: "classifier",
				Reason: "classified as complex task (keyword: " + kw + ")",
			}
		}
	}

	// Very long messages → premium model (needs deep context understanding)
	if length > 500 {
		return &RoutingDecision{
			Model:  c.router.defaultModel,
			Source: "classifier",
			Reason: "long message, using default model",
		}
	}

	// Simple tasks → fast/cheap model
	if c.router.fastModel != "" {
		return &RoutingDecision{
			Model:  c.router.fastModel,
			Source: "classifier",
			Reason: "classified as simple task, using fast model",
		}
	}

	return nil // fallback to default
}
