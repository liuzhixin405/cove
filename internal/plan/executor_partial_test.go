package plan

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/delegate"
	"github.com/liuzhixin405/cove/internal/tool"
)

// busyProvider never finishes: every answer asks for another (distinct) tool
// call, so a sub-agent runs until its model-call cap.
type busyProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *busyProvider) Name() string        { return "busy" }
func (p *busyProvider) DisplayName() string { return "busy" }
func (p *busyProvider) Validate() error     { return nil }
func (p *busyProvider) Chat(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	return &api.ChatResponse{
		Content:   fmt.Sprintf("第 %d 步", n),
		ToolCalls: []api.ToolCall{{ID: fmt.Sprintf("t%d", n), Name: "read", Input: map[string]any{"path": fmt.Sprintf("f%d", n)}}},
	}, nil
}
func (p *busyProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func cappedExecutor(t *testing.T, p *busyProvider) *PlanExecutor {
	t.Helper()
	rt := &tool.Runtime{Tasks: map[string]*tool.TaskRecord{"a": {ID: "a", Status: "pending"}}}
	d := delegate.NewDelegator(p, "m", nil)
	d.SetMaxIter(2)
	pe := NewPlanExecutor(d, rt)
	pe.SetMaxRetries(1)
	return pe
}

// A task whose sub-agent hit its cap keeps what the sub-agent did, and is not
// re-run from scratch: a retry would hit the same cap after redoing the work.
func TestExecuteKeepsPartialOutputAndDoesNotRetryAtCap(t *testing.T) {
	p := &busyProvider{}
	pe := cappedExecutor(t, p)
	plan := &Plan{ID: "p", Tasks: []*Task{{ID: "a", Title: "A", Description: "A", Status: "pending"}}}

	res := pe.Execute(context.Background(), plan)
	task := res.Tasks[0]
	if task.Status != "failed" || !strings.Contains(task.Error, "已达上限") {
		t.Fatalf("task = %s (%s), want failed at the cap", task.Status, task.Error)
	}
	if !strings.Contains(task.Output, "已完成 2 个步骤") || !strings.Contains(task.Output, "第 2 步") {
		t.Fatalf("partial output lost: %q", task.Output)
	}
	if p.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (no retry after the cap)", p.calls)
	}
	if s := FormatResult(res); !strings.Contains(s, "已完成 2 个步骤") {
		t.Fatalf("FormatResult hides the partial output:\n%s", s)
	}
}
