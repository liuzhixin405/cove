package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// The bash tool appends stderr and the exit code after stdout. Keeping only
// the head of a long result dropped both, and the model read a failed build as
// a success.
func TestLongBashResultKeepsExitCode(t *testing.T) {
	long := strings.Repeat("ok  \tgithub.com/example/pkg\t0.01s\n", 5000) +
		"\n[stderr]\n--- FAIL: TestLogin\n\n[exit code: 1]"
	bash := &mockTool{name: "bash", readOnly: true, result: long}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "b1", Name: "bash", Input: map[string]any{"command": "go test ./..."}}}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, bash)
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "run tests"}, nil, nil); err != nil {
		t.Fatal(err)
	}

	var got string
	for _, m := range prov.lastReq.Messages {
		if m.Role == "tool" {
			got = m.Content
		}
	}
	if got == "" {
		t.Fatal("no tool result reached the model")
	}
	for _, want := range []string{"[exit code: 1]", "--- FAIL: TestLogin", "ok  \tgithub.com/example/pkg"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool result sent to the model lacks %q", want)
		}
	}
	if len(got) >= len(long) {
		t.Errorf("tool result was not shortened (%d bytes)", len(got))
	}
}
