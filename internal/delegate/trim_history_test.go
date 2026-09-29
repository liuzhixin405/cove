package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

func TestTrimHistoryCutsOldToolResultsOnly(t *testing.T) {
	big := strings.Repeat("x", 30000)
	msgs := []api.Message{{Role: "user", Content: "task"}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "c", Name: "read"}}},
			api.Message{Role: "tool", ToolCallID: "c", Content: big})
	}
	out := trimHistory(msgs, 40000)
	var trimmed, full int
	for _, m := range out {
		if m.Role != "tool" {
			continue
		}
		if strings.HasSuffix(m.Content, trimmedToolNote) {
			trimmed++
		} else {
			full++
		}
	}
	if trimmed == 0 || full < keepRecentToolResults {
		t.Fatalf("trimmed %d, full %d", trimmed, full)
	}
	if last := out[len(out)-1].Content; last != big {
		t.Fatal("the latest tool result was trimmed")
	}
	if got := trimHistory([]api.Message{{Role: "user", Content: "hi"}}, 10); len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("small history changed: %+v", got)
	}
}

type captureProvider struct {
	api.Provider
	system string
}

func (p *captureProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.system = req.SystemBase
	return &api.ChatResponse{Content: "done"}, nil
}

func TestDelegateReportsProgress(t *testing.T) {
	d := NewDelegator(&captureProvider{}, "m", nil)
	var lines []string
	d.SetProgress(func(l string) { lines = append(lines, l) })
	d.DelegateWith(context.Background(), "agent-explore-1", "find the parser", "sys", Options{})
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "▸ agent-explore-1 开始：find the parser") || !strings.HasPrefix(lines[1], "✓ agent-explore-1 完成") {
		t.Fatalf("progress = %q", lines)
	}
}

type toolsProvider struct {
	api.Provider
	tools []string
}

func (p *toolsProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	for _, d := range req.Tools {
		p.tools = append(p.tools, d.Name)
	}
	return &api.ChatResponse{Content: "done"}, nil
}

// A code review gets no web tools: it spent seven searches on the
// definition of a median.
func TestDelegateExcludesTools(t *testing.T) {
	p := &toolsProvider{}
	d := NewDelegator(p, "m", []tool.Tool{&namedTool{name: "read", readOnly: true}, &namedTool{name: "websearch", readOnly: true}, &namedTool{name: "grep", readOnly: true}})
	d.DelegateWith(context.Background(), "r1", "review", "sys", Options{ReadOnly: true, Exclude: []string{"websearch"}})
	if strings.Join(p.tools, ",") != "read,grep" {
		t.Fatalf("tools offered = %v", p.tools)
	}
}

type overloadedProvider struct {
	api.Provider
	models []string
}

func (p *overloadedProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.models = append(p.models, req.Model)
	if req.Model == "main" {
		return nil, &api.RetryableError{Status: 503, Msg: "high demand"}
	}
	return &api.ChatResponse{Content: "done"}, nil
}

// A sub-agent whose model is overloaded moves to the fallback once instead
// of dying on the first 503.
func TestSubAgentFallsBackOnOverload(t *testing.T) {
	p := &overloadedProvider{}
	d := NewDelegator(p, "main", nil)
	d.SetFallback(func(m string) string {
		if m == "main" {
			return "fast"
		}
		return ""
	})
	var lines []string
	d.SetProgress(func(l string) { lines = append(lines, l) })
	res := d.DelegateWith(context.Background(), "a1", "task", "sys", Options{})
	if !res.Success || strings.Join(p.models, ",") != "main,fast" {
		t.Fatalf("result %+v, models %v", res, p.models)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "改用 fast") {
		t.Fatalf("progress = %q", lines)
	}
}

func TestDelegateAppendsProjectContext(t *testing.T) {
	p := &captureProvider{}
	d := NewDelegator(p, "m", nil)
	d.SetContextSource(func() string { return "# Project context\n\nAGENTS rule" })
	d.DelegateWith(context.Background(), "t1", "do it", "You are a sub-agent.", Options{})
	if !strings.HasPrefix(p.system, "You are a sub-agent.") || !strings.Contains(p.system, "AGENTS rule") {
		t.Fatalf("system prompt = %q", p.system)
	}
}
