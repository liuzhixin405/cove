package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// panicTurn runs one turn whose first response is calls, and returns the
// tool results the engine recorded. A panic that escapes the engine fails the
// test binary, which is the bug these tests pin.
func panicTurn(t *testing.T, calls []api.ToolCall, tools ...*mockTool) []api.Message {
	t.Helper()
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return &api.ChatResponse{ToolCalls: calls}, nil
		}
		return &api.ChatResponse{Content: "完成"}, nil
	}}
	var ts []tool.Tool
	for _, m := range tools {
		ts = append(ts, m)
	}
	eng := newPatternEngine(t, prov, nil, ts...)
	if _, err := run(t, eng, "跑工具"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	var out []api.Message
	for _, m := range eng.messages {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

func requirePanicResult(t *testing.T, results []api.Message, id string) {
	t.Helper()
	for _, r := range results {
		if r.ToolCallID == id {
			if !strings.HasPrefix(r.Content, "Error: tool panicked") {
				t.Fatalf("result of %s = %q, want the panic as an error", id, r.Content)
			}
			return
		}
	}
	t.Fatalf("no result for %s in %+v", id, results)
}

// One call takes the sequential path.
func TestToolPanicRecoveredOnSingleCall(t *testing.T) {
	boom := &mockTool{name: "boom", panicMsg: "nil map"}
	res := panicTurn(t, []api.ToolCall{{ID: "p1", Name: "boom", Input: map[string]any{}}}, boom)
	requirePanicResult(t, res, "p1")
}

// A non-concurrency-safe call in a batch runs inline, as a barrier.
func TestToolPanicRecoveredOnSerialCall(t *testing.T) {
	boom := &mockTool{name: "boom", panicMsg: "nil map"}
	ok := &mockTool{name: "look", readOnly: true, safe: true, result: "fine"}
	res := panicTurn(t, []api.ToolCall{
		{ID: "a", Name: "look", Input: map[string]any{}},
		{ID: "p2", Name: "boom", Input: map[string]any{}},
	}, boom, ok)
	requirePanicResult(t, res, "p2")
}

// A second write to the same file is deferred until the batch drains.
func TestToolPanicRecoveredOnDeferredCall(t *testing.T) {
	w := &mockTool{name: "write", panicMsg: "disk gone"}
	res := panicTurn(t, []api.ToolCall{
		{ID: "w1", Name: "write", Input: map[string]any{"file_path": "same.txt"}},
		{ID: "w2", Name: "write", Input: map[string]any{"file_path": "same.txt"}},
	}, w)
	requirePanicResult(t, res, "w1")
	requirePanicResult(t, res, "w2")
}
