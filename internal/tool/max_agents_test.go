package tool

import (
	"context"
	"testing"
)

func TestExecutePlanPassesClampedMaxAgents(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{nil, DefaultMaxAgents}, {float64(2), 2}, {float64(0), 1}, {float64(20), MaxAgentsLimit}, {float64(8), 8},
	}
	for _, c := range cases {
		var got int
		rt := &Runtime{PlanExecuteFunc: func(ctx context.Context, parallel bool) (string, error) {
			got = MaxAgentsFrom(ctx, -1)
			return "ok", nil
		}}
		in := Input{"parallel": true}
		if c.in != nil {
			in["max_agents"] = c.in
		}
		if _, err := NewExecutePlanTool().Call(context.Background(), in, Context{Runtime: rt}); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("max_agents %v -> %d, want %d", c.in, got, c.want)
		}
	}
	if MaxAgentsFrom(context.Background(), 4) != 4 {
		t.Error("missing value should return the fallback")
	}
}
