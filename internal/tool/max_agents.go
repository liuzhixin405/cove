package tool

import "context"

// execute_plan's max_agents bounds.
const (
	DefaultMaxAgents = 4
	MaxAgentsLimit   = 8
)

type maxAgentsKey struct{}

// WithMaxAgents returns ctx carrying the execute_plan concurrency cap,
// clamped to 1..MaxAgentsLimit. The plan executor reads it with MaxAgentsFrom.
func WithMaxAgents(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, maxAgentsKey{}, clampMaxAgents(n))
}

// MaxAgentsFrom returns the cap set by WithMaxAgents, or fallback when none is set.
func MaxAgentsFrom(ctx context.Context, fallback int) int {
	if n, ok := ctx.Value(maxAgentsKey{}).(int); ok {
		return n
	}
	return fallback
}

func clampMaxAgents(n int) int {
	if n < 1 {
		return 1
	}
	if n > MaxAgentsLimit {
		return MaxAgentsLimit
	}
	return n
}
