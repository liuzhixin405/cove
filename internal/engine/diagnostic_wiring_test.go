package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/diagnostic"
)

const llamaOverflowMsg = `{"error":{"code":400,"message":"request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error","n_ctx":16384}}`

// clearDiagnostics empties the recorded events and disables remedies.
func clearDiagnostics(t *testing.T) {
	t.Helper()
	diagnostic.ResetForTest()
	t.Cleanup(diagnostic.ResetForTest)
}

// A turn that dies of context overflow leaves exactly one coded E2008 event
// (no uncoded duplicate from a log line), carrying the model, and names the
// code in the interruption reason the user sees.
func TestContextOverflowIsReportedOnceWithItsCode(t *testing.T) {
	clearDiagnostics(t)
	overflow := &api.StatusError{Status: 400, Msg: llamaOverflowMsg}
	prov := &mockProvider{responses: []mockResponse{{err: overflow}, {err: overflow}, {err: overflow}}}
	eng := newTestEngine(prov)

	_, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "hi"}, nil, nil)
	if err == nil {
		t.Fatal("want the overflow error back")
	}
	coded := 0
	for _, ev := range diagnostic.RecentRuntime() {
		if ev.Code == diagnostic.ErrAPIContextLength {
			coded++
			// The model the router chose for the call (newTestEngine's).
			if ev.Model != "test-model" {
				t.Errorf("event lacks the routed model: %+v", ev)
			}
		}
	}
	if coded != 1 {
		t.Fatalf("E2008 recorded %d times, want 1: %+v", coded, diagnostic.RecentRuntime())
	}
	if eng.interrupted == nil || !strings.Contains(eng.interrupted.reason, "[E2008]") {
		t.Errorf("interruption reason does not name the code: %+v", eng.interrupted)
	}
}

// A tool call whose arguments were not JSON is reported as E4009 with the
// tool and model.
func TestUnparsableToolArgsAreReported(t *testing.T) {
	clearDiagnostics(t)
	eng := newTestEngine(&mockProvider{})
	eng.config.Model = "qwen3.6-27b"
	out, _ := eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "bash", ParseError: true,
		Input: map[string]any{"_cove_parse_error": "tool call arguments were not valid JSON"}})
	if !strings.HasPrefix(out, "Error:") {
		t.Fatalf("result = %q", out)
	}
	evs := diagnostic.RecentRuntime()
	if len(evs) != 1 || evs[0].Code != diagnostic.ErrToolArgsInvalid || evs[0].Tool != "bash" || evs[0].Model != "qwen3.6-27b" {
		t.Fatalf("events = %+v", evs)
	}
}

// A stalled stage is E5007, not a free-text log line.
func TestStallIsReportedWithItsCode(t *testing.T) {
	clearDiagnostics(t)
	eng := newTestEngine(&mockProvider{})
	eng.reportStall("call model qwen3.6-27b", 33e9, true)
	evs := diagnostic.RecentRuntime()
	if len(evs) != 1 || evs[0].Code != diagnostic.ErrEngineStall || !strings.Contains(evs[0].Message, "call model qwen3.6-27b") {
		t.Fatalf("events = %+v", evs)
	}
}
