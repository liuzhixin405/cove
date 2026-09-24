package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// A sub-agent that ran out of its time budget was reported as "cancelled",
// which reads as if the user pressed Ctrl+C; the plan summary then showed
// "(cancelled)" for a task nobody cancelled.
func TestSubAgentReportsTimeoutAsTimeout(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "never"}}}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	res := NewSubAgent(Config{Provider: prov, Model: "m"}).Run(ctx, "task", "sys")
	if res.Success || !strings.Contains(res.Error, "timed out") {
		t.Fatalf("result = %+v, want a timed-out failure", res)
	}
}

func TestSubAgentReportsUserCancelAsCancelled(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "never"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := NewSubAgent(Config{Provider: prov, Model: "m"}).Run(ctx, "task", "sys")
	if res.Success || res.Error != "cancelled" {
		t.Fatalf("result = %+v, want cancelled", res)
	}
}

// worktree/exit_worktree switch the session's active worktree, which is the
// parent's state, not the sub-agent's.
func TestSubAgentCannotSwitchTheSessionWorktree(t *testing.T) {
	var tools []tool.Tool
	for _, n := range []string{"read", "worktree", "exit_worktree"} {
		tools = append(tools, &namedTool{name: n, readOnly: true})
	}
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "done"}}}
	NewSubAgent(Config{Provider: prov, Model: "m", Tools: tools}).Run(context.Background(), "task", "sys")

	if got := toolNames(prov.lastReq.Tools); len(got) != 1 || got[0] != "read" {
		t.Fatalf("sub-agent was offered %v, want only [read]", got)
	}
}
