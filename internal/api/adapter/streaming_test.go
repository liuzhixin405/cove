package adapter

import "testing"

func TestStreamAccumulatorAggregatesContentReasoningAndToolCalls(t *testing.T) {
	var acc StreamAccumulator
	acc.AddDelta("hello")
	acc.AddDelta(" world")
	acc.AddReasoning("step1")
	acc.AddReasoning("\nstep2")
	acc.AddToolCall(ToolCall{ID: "1", Name: "read", Input: map[string]any{"filePath": "README.md"}})

	if got, want := acc.Content(), "hello world"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if got, want := acc.Reasoning(), "step1\nstep2"; got != want {
		t.Fatalf("reasoning = %q, want %q", got, want)
	}
	calls := acc.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "read" {
		t.Fatalf("tool name = %q, want read", calls[0].Name)
	}
}

func TestCloneToolCallsAndParseErrorHelpers(t *testing.T) {
	orig := []ToolCall{
		{ID: "1", Name: "ok"},
		{ID: "2", Name: "bad", ParseError: true},
	}
	cp := CloneToolCalls(orig)
	if len(cp) != len(orig) {
		t.Fatalf("clone length = %d, want %d", len(cp), len(orig))
	}
	if &cp[0] == &orig[0] {
		t.Fatal("clone should allocate a new slice")
	}
	if !HasParseError(cp) {
		t.Fatal("expected parse error detection")
	}
}

func TestMergeReasoningSkipsEmptyParts(t *testing.T) {
	got := MergeReasoning("", " first ", "", "second")
	if got != "first\nsecond" {
		t.Fatalf("MergeReasoning = %q, want first\\nsecond", got)
	}
}
