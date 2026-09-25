package engine

import (
	"context"
	"errors"

	"github.com/liuzhixin405/cove/internal/api"
)

// generateOnceMaxTokens bounds the answer of a GenerateOnce call.
const generateOnceMaxTokens = 4096

// errBudgetSpent is GenerateOnce's error once max_budget_usd is reached.
var errBudgetSpent = errors.New("budget exhausted")

// GenerateOnce makes one tool-less model call with system and prompt and
// returns the answer (/init drafts CLAUDE.md with it). It goes through the
// fallback chain, so it is billed like any turn, and it never touches the
// session history. It is skipped when ctx is done or the budget is spent.
func (e *Engine) GenerateOnce(ctx context.Context, system, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.costTracker != nil && e.costTracker.OverBudget() {
		return "", errBudgetSpent
	}
	req := api.ChatRequest{
		Model:      e.config.Model,
		SystemBase: system,
		Messages:   []api.Message{{Role: "user", Content: prompt}},
		MaxTokens:  generateOnceMaxTokens,
	}
	resp, _, err := e.fallback.TryChat(ctx, func(api.Provider) api.ChatRequest { return req })
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
