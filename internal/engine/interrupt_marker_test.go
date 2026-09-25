package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/tool"
)

const markerPrefix = "[system: The previous turn was interrupted ("

func TestInterruptMarkerWrittenOncePerInterruption(t *testing.T) {
	var finish atomic.Bool
	prov := &seqProvider{reply: func(_ context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if finish.Load() {
			return &api.ChatResponse{Content: "全部完成了，修改已经写入并检查过。"}, nil
		}
		return toolCallResp(fmt.Sprintf("c%d", n), "read_tool", map[string]any{"query": fmt.Sprintf("q%d", n)}), nil
	}}
	eng := limitEngine(t, prov, 2) // -p style: every turn stops at 2 calls

	if _, err := run(t, eng, "做点事"); err == nil {
		t.Fatal("want a stop at the cap")
	}
	if _, err := run(t, eng, "做点事"); err == nil { // /continue, stopped again
		t.Fatal("want a second stop")
	}
	if got := countMsgs(eng.messages, markerPrefix); got != 1 {
		t.Fatalf("markers after two interruptions of one turn = %d, want 1", got)
	}
	var marker api.Message
	for _, m := range eng.messages {
		if strings.HasPrefix(m.Content, markerPrefix) {
			marker = m
		}
	}
	if marker.Role != "user" || !marker.Synthetic || !strings.Contains(marker.Content, "re-check state before repeating work") {
		t.Fatalf("marker = %+v", marker)
	}

	finish.Store(true)
	if _, err := run(t, eng, "做点事"); err != nil { // the resume completes
		t.Fatalf("resume: %v", err)
	}
	finish.Store(false)
	if _, err := run(t, eng, "再做点事"); err == nil {
		t.Fatal("want a stop at the cap")
	}
	if got := countMsgs(eng.messages, markerPrefix); got != 2 {
		t.Fatalf("markers after a completed resume and a new interruption = %d, want 2", got)
	}
}

func TestInterruptMarkerOnCancel(t *testing.T) {
	prov := &seqProvider{}
	eng := newPatternEngine(t, prov, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.RunMessageWithStream(ctx, api.Message{Role: "user", Content: "hi"}, nil, nil); err == nil {
		t.Fatal("want the cancel error")
	}
	if got := countMsgs(eng.messages, markerPrefix+"user cancel)"); got != 1 {
		t.Fatalf("cancel marker count = %d, want 1", got)
	}
}

func TestPermissionDenialTellsModelNotToRetry(t *testing.T) {
	writeTool := &mockTool{name: "write", result: "written"}
	eng := newTestEngine(&mockProvider{}, writeTool)
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return false }
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)
	out := eng.executeTool(context.Background(), api.ToolCall{ID: "t1", Name: "write", Input: map[string]any{"filePath": "a.go"}})
	for _, want := range []string{
		"Error: permission denied for write (user rejected).",
		"Do not call this tool again with the same input",
		"explain the situation to the user or choose a different approach",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("denial %q lacks %q", out, want)
		}
	}
	eng.perm.SetMode(permission.Plan)
	out = eng.executeTool(context.Background(), api.ToolCall{ID: "t2", Name: "write", Input: map[string]any{"filePath": "a.go"}})
	if !strings.Contains(out, "Do not call this tool again") {
		t.Fatalf("plan-mode denial %q lacks the do-not-retry instruction", out)
	}
}

// A different request that is interrupted too leaves its own marker, even
// though the earlier interruption was never resumed.
func TestInterruptMarkerNewRequestAfterInterruption(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		return toolCallResp(fmt.Sprintf("c%d", n), "read_tool", map[string]any{"query": fmt.Sprintf("q%d", n)}), nil
	}}
	eng := limitEngine(t, prov, 2)
	_, _ = run(t, eng, "任务一")
	_, _ = run(t, eng, "任务二")
	if got := countMsgs(eng.messages, markerPrefix+"iteration limit)"); got != 2 {
		t.Fatalf("iteration-limit markers = %d, want 2 (one per request)", got)
	}
	if got := countMsgs(eng.messages, "interrupted by the user"); got != 0 {
		t.Fatalf("a limit stop is not a user interruption; %d markers say so", got)
	}
}

func TestInterruptMarkerReasons(t *testing.T) {
	cases := map[string]string{
		"已被用户取消":         "user cancel",
		"预算已用尽":          "budget exhausted",
		"模型调用失败: 502":    "API error",
		"达到单轮最大迭代次数":     "iteration limit",
		"达到单轮时间上限":       "time limit",
		"检测到操作循环":        "loop detected",
		"检测到操作循环，用户选择停止": "loop detected",
		"疑似停滞，用户选择停止":    "stagnation",
	}
	for in, want := range cases {
		if got := interruptMarkerReason(in); got != want {
			t.Errorf("interruptMarkerReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPermissionDenialPolicyAndNoHandlerTellModelNotToRetry(t *testing.T) {
	writeTool := &mockTool{name: "write", result: "written"}
	eng := newTestEngine(&mockProvider{}, writeTool)
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)
	eng.PermissionPrompt = nil
	err := eng.authorizeToolCall(api.ToolCall{ID: "t", Name: "write", Input: map[string]any{"filePath": "a.go"}}, tool.Context{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no interactive approval handler") || !strings.Contains(err.Error(), "Do not call this tool again with the same input") {
		t.Fatalf("no-handler denial = %v", err)
	}
	if got := policyDenied("write").Error(); !strings.Contains(got, "denied by policy for write") || !strings.Contains(got, "Do not call this tool again with the same input") {
		t.Fatalf("policy denial = %q", got)
	}
}
