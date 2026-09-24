package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// The guardrail counters were never reset, so a tool that failed eight times
// at any point in the session (a build that kept failing while being fixed)
// stayed blocked for the rest of the session: each blocked call counts as
// another failure, so the count could never recover.
func TestGuardrailCountersResetForEachUserTurn(t *testing.T) {
	bash := &mockTool{name: "bash", readOnly: true, result: "ok"}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "b1", Name: "bash", Input: map[string]any{"command": "go build ./..."}}}},
		{content: "built"},
	}}
	eng := newTestEngine(prov, bash)
	for i := 0; i < 10; i++ {
		eng.guardrails.AfterCall("bash", map[string]any{"command": "go build ./..."}, "Error: build failed", true)
	}

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "try the build again"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if bash.callCount != 1 {
		t.Fatalf("bash ran %d times in a new turn after earlier failures, want 1", bash.callCount)
	}
}
