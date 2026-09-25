package plan

import (
	"context"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/tool"
)

// execute_plan's max_agents reaches the executor through the context and caps
// how many sibling sub-agents run at once.
func TestExecuteHonoursMaxAgentsFromContext(t *testing.T) {
	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "done", nil
	}
	pe, _ := newExecutor(t, p, "a", "b", "c", "d")
	pl := &Plan{ID: "p", Parallel: true, Tasks: []*Task{
		{ID: "a", Description: "a", Status: "pending"},
		{ID: "b", Description: "b", Status: "pending"},
		{ID: "c", Description: "c", Status: "pending"},
		{ID: "d", Description: "d", Status: "pending"},
	}}
	res := pe.Execute(tool.WithMaxAgents(context.Background(), 1), pl)
	if !res.Success {
		t.Fatalf("Execute failed: %+v", statusByID(res))
	}
	if got := p.peakConcurrency(); got != 1 {
		t.Fatalf("peak concurrency = %d, want 1 (max_agents=1)", got)
	}
}
