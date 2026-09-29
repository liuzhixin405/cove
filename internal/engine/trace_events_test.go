package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/trace"
)

// One turn with a tool call leaves a readable trail: the turn, each model
// call with its size and duration, each tool call with its outcome. This is
// the log that lets a "task never finished" be read back instead of
// reconstructed from session files.
func TestATurnLeavesTurnModelAndToolTraceEvents(t *testing.T) {
	trace.SetPath(filepath.Join(t.TempDir(), "trace.jsonl"))
	t.Cleanup(func() { trace.SetPath("") })

	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "echo hi"}}}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, &mockTool{name: "bash", safe: true, readOnly: true, result: "hi"})
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "run it"}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	events, err := trace.Tail(50)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	if kinds["turn"] != 1 || kinds["model"] != 2 || kinds["tool"] != 1 {
		t.Fatalf("event kinds = %v, want 1 turn, 2 model, 1 tool", kinds)
	}
	for _, ev := range events {
		switch ev.Kind {
		case "model":
			if ev.Fields["model"] != "test-model" || ev.Fields["ms"] == nil || ev.Fields["est_tokens"] == nil {
				t.Errorf("model event lacks model/ms/est_tokens: %v", ev.Fields)
			}
		case "tool":
			if ev.Fields["name"] != "bash" || ev.Fields["error"] != false || ev.Fields["result_bytes"] != float64(2) {
				t.Errorf("tool event fields = %v", ev.Fields)
			}
		case "turn":
			if ev.Fields["user_bytes"] != float64(len("run it")) || ev.Fields["window"] == nil {
				t.Errorf("turn event fields = %v", ev.Fields)
			}
		}
	}
	for _, ev := range events {
		for k, v := range ev.Fields {
			if s, ok := v.(string); ok && len(s) > 200 {
				t.Errorf("trace field %s carries %d bytes of text; the trace records sizes, not content", k, len(s))
			}
		}
	}
}
