package tool

import (
	"context"
	"testing"
)

type planCtxKey struct{}

// The plan executor runs sub-agents for minutes. It has to receive the
// caller's context, or cancelling the turn (Ctrl+C) cannot stop them.
func TestExecutePlanPassesTheCallersContext(t *testing.T) {
	var got context.Context
	rt := &Runtime{PlanExecuteFunc: func(ctx context.Context, parallel bool) (string, error) {
		got = ctx
		return "ok", nil
	}}
	ctx := context.WithValue(context.Background(), planCtxKey{}, "turn")

	if _, err := NewExecutePlanTool().Call(ctx, Input{"parallel": true}, Context{Runtime: rt}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Value(planCtxKey{}) != "turn" {
		t.Fatalf("plan executor did not receive the caller's context")
	}
}
