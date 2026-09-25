package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

type errProvider struct{ fakeProvider }

func (p *errProvider) Chat(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
	return nil, errors.New("upstream 500")
}
func (p *errProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func TestSubAgentExitReasonCompleted(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "all done"}}}
	res := NewSubAgent(Config{Provider: prov, Model: "m"}).Run(context.Background(), "task", "sys")
	if res.ExitReason != ExitCompleted || res.Truncated || res.CapReached {
		t.Fatalf("result = %+v, want completed, not truncated", res)
	}
}

func TestSubAgentExitReasonMaxIterations(t *testing.T) {
	sa := NewSubAgent(Config{Provider: &stepProvider{}, MaxIter: 2, Tools: []tool.Tool{&probeTool{readOnly: true}}})
	res := sa.Run(context.Background(), "task", "")
	if res.ExitReason != ExitMaxIterations || !res.Truncated || !res.CapReached {
		t.Fatalf("result = %+v, want max_iterations, truncated, CapReached", res)
	}
}

func TestSubAgentExitReasonInterrupted(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "never"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := NewSubAgent(Config{Provider: prov, Model: "m"}).Run(ctx, "task", "sys")
	if res.ExitReason != ExitInterrupted || res.CapReached {
		t.Fatalf("result = %+v, want interrupted", res)
	}
	budget := NewSubAgent(Config{Provider: prov, Model: "m", BudgetExceeded: func() bool { return true }}).
		Run(context.Background(), "task", "sys")
	if budget.ExitReason != ExitInterrupted {
		t.Fatalf("budget stop = %+v, want interrupted", budget)
	}
}

func TestSubAgentExitReasonError(t *testing.T) {
	res := NewSubAgent(Config{Provider: &errProvider{}, Model: "m"}).Run(context.Background(), "task", "sys")
	if res.ExitReason != ExitError || res.Success {
		t.Fatalf("result = %+v, want error", res)
	}
}

func TestSubAgentExitReasonLoop(t *testing.T) {
	read := &namedTool{name: "read", readOnly: true}
	var responses []*api.ChatResponse
	for i := 0; i < 5; i++ {
		responses = append(responses, &api.ChatResponse{ToolCalls: []api.ToolCall{
			{ID: "x", Name: "read", Input: map[string]any{"path": "a.go"}},
		}})
	}
	res := NewSubAgent(Config{Provider: &fakeProvider{responses: responses}, Model: "m", Tools: []tool.Tool{read}}).
		Run(context.Background(), "task", "sys")
	if res.ExitReason != ExitLoop {
		t.Fatalf("result = %+v, want loop", res)
	}
}
