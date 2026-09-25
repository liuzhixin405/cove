package delegate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// stepProvider asks for a distinct read call on every request, saying what it
// is about to do, so the loop guard never fires and the cap is what stops it.
type stepProvider struct{ n int }

func (p *stepProvider) Name() string        { return "step" }
func (p *stepProvider) DisplayName() string { return "step" }
func (p *stepProvider) Validate() error     { return nil }
func (p *stepProvider) Chat(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
	p.n++
	return &api.ChatResponse{
		Content:   fmt.Sprintf("正在查看第 %d 个文件", p.n),
		ToolCalls: []api.ToolCall{{ID: fmt.Sprintf("t%d", p.n), Name: "write", Input: map[string]any{"filePath": fmt.Sprintf("f%d.go", p.n)}}},
	}, nil
}
func (p *stepProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func TestSubAgentDefaultMaxIterIs60(t *testing.T) {
	if DefaultMaxIter != 60 {
		t.Fatalf("DefaultMaxIter = %d, want 60", DefaultMaxIter)
	}
	sa := NewSubAgent(Config{Provider: &stepProvider{}})
	if sa.maxIter != 60 {
		t.Fatalf("maxIter = %d, want 60", sa.maxIter)
	}
}

func TestSubAgentCapReturnsPartialResult(t *testing.T) {
	prov := &stepProvider{}
	sa := NewSubAgent(Config{
		Provider: prov, MaxIter: 3,
		Tools: []tool.Tool{&probeTool{readOnly: true}},
	})
	res := sa.Run(context.Background(), "看文件", "")
	if res.Success {
		t.Fatal("a capped run is not a success")
	}
	if !strings.Contains(res.Error, "已达上限") || !strings.Contains(res.Error, "以下为部分结果") {
		t.Fatalf("Error = %q", res.Error)
	}
	if res.Steps != 3 {
		t.Fatalf("Steps = %d, want 3", res.Steps)
	}
	for _, want := range []string{"f1.go", "f3.go", "正在查看第 3 个文件"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("partial Output lacks %q:\n%s", want, res.Output)
		}
	}
}

func TestDelegatorUsesConfiguredMaxIter(t *testing.T) {
	prov := &stepProvider{}
	d := NewDelegator(prov, "m", []tool.Tool{&probeTool{readOnly: true}})
	d.SetMaxIter(2)
	res := d.Delegate(context.Background(), "t", "看文件", "")
	if prov.n != 2 || res.Steps != 2 {
		t.Fatalf("model calls = %d, steps = %d, want 2", prov.n, res.Steps)
	}
	d.SetMaxIter(0) // back to the default
	if d.maxIterOrDefault() != DefaultMaxIter {
		t.Fatalf("SetMaxIter(0) should restore the default, got %d", d.maxIterOrDefault())
	}
}
