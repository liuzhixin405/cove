package command

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/trace"
)

func TestFormatTraceEventRendersEachKindOnOneLine(t *testing.T) {
	at := time.Date(2026, 9, 27, 15, 34, 11, 0, time.Local)
	cases := []struct {
		ev   trace.Event
		want []string
	}{
		{trace.Event{Time: at, Kind: "model", Fields: map[string]any{"model": "qwen3.6-27b", "messages": 12.0, "est_tokens": 17773.0, "ms": 226000.0,
			"error": "API error 400: request (17773 tokens) exceeds", "error_kind": "context_length"}},
			[]string{"15:34:11 model", "qwen3.6-27b", "12 msgs", "17773 tok", "3m46s", "context_length", "exceeds"}},
		{trace.Event{Time: at, Kind: "model", Fields: map[string]any{"model": "m", "messages": 3.0, "est_tokens": 900.0, "ms": 1500.0, "stop": "tool_calls", "in": 800.0, "out": 40.0, "tool_calls": 2.0, "content_bytes": 0.0}},
			[]string{"1.5s", "tool_calls", "in 800", "out 40", "2 工具调用"}},
		{trace.Event{Time: at, Kind: "tool", Fields: map[string]any{"name": "write", "ms": 3.0, "result_bytes": 69.0, "error": true, "head": "Error: path outside working directory: D:\\x"}},
			[]string{"tool", "write", "69 B", "✗", "path outside working directory"}},
		{trace.Event{Time: at, Kind: "compact", Fields: map[string]any{"tokens_before": 17773.0, "tokens_after": 9800.0, "msgs_before": 14.0, "msgs_after": 6.0, "compressed": true, "summarized": true}},
			[]string{"compact", "17773 → 9800 tok", "14 → 6 msgs", "已摘要早期对话"}},
		{trace.Event{Time: at, Kind: "overflow", Fields: map[string]any{"model": "m", "tokens_before": 17773.0, "tokens_after": 17773.0, "retry": false}},
			[]string{"overflow", "无法再缩小"}},
		{trace.Event{Time: at, Kind: "turn", Fields: map[string]any{"model": "m", "user_bytes": 50.0, "note_bytes": 15670.0, "messages": 2.0, "est_tokens": 11000.0, "overhead_tokens": 6000.0, "window": 16384.0}},
			[]string{"turn", "50 B", "15.3 KB", "11000 tok", "6000", "16384"}},
	}
	for _, c := range cases {
		line := FormatTraceEvent(c.ev)
		if strings.Contains(line, "\n") {
			t.Errorf("%s event rendered on several lines: %q", c.ev.Kind, line)
		}
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s line lacks %q:\n%s", c.ev.Kind, w, line)
			}
		}
	}
}

func TestDiagnoseTraceShowsTheLastEvents(t *testing.T) {
	trace.SetPath(filepath.Join(t.TempDir(), "trace.jsonl"))
	t.Cleanup(func() { trace.SetPath("") })
	for i := 0; i < 5; i++ {
		trace.Write("tool", map[string]any{"name": "read", "ms": i, "result_bytes": 10, "error": false})
	}
	out, err := (&diagnoseCmd{}).Execute(context.Background(), Input{Args: []string{"trace", "2"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.Message, " tool "); n != 2 {
		t.Fatalf("want 2 tool lines, got %d:\n%s", n, out.Message)
	}
	if !strings.Contains(out.Message, "最近 2 条") {
		t.Fatalf("header missing:\n%s", out.Message)
	}
}
