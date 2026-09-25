package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// Important 1: when a nudge keeps the turn going, the second reply is
// streamed after the first; a blank line must separate them.
func TestNudgedRepliesAreSeparatedInTheStream(t *testing.T) {
	const second = "修改了 a.go 的解析逻辑，并补充了对应的单元测试，没有遗留问题需要处理。"
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		switch n {
		case 0:
			return toolCallResp("c0", "write", map[string]any{"input": "x"}), nil
		case 1:
			return &api.ChatResponse{Content: "好的。"}, nil // degenerate: nudged
		}
		return &api.ChatResponse{Content: second}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) {
		c.DoneCheck = "off"
		c.LoopDetectionDisabled = true
	}, &mockTool{name: "write", result: "written"})
	var mu sync.Mutex
	var stream strings.Builder
	onDelta := func(d string) { mu.Lock(); stream.WriteString(d); mu.Unlock() }
	got, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "改 a.go"}, onDelta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != second {
		t.Fatalf("final text = %q, want only the second reply", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(stream.String(), "好的。\n\n"+second) {
		t.Fatalf("stream = %q, want a blank line between the two replies", stream.String())
	}
}

// Important 1: -p (no onDelta) is unchanged — emitSeparator writes nothing.
func TestEmitSeparatorWithoutStreamIsSilent(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	var lines []string
	eng.OnEngineOutput = func(s string) { lines = append(lines, s) }
	eng.emitSeparator(nil, true)
	if len(lines) != 0 {
		t.Fatalf("engine output without a stream: %q", lines)
	}
	var got string
	eng.emitSeparator(func(d string) { got += d }, true)
	if got != "\n\n" || len(lines) != 1 || !strings.Contains(lines[0], "自检中") {
		t.Fatalf("delta %q, lines %q", got, lines)
	}
}

// Minor 1: "让我们……" invites the reader and announces nothing.
func TestLetUsReviewIsNotAnAnnouncement(t *testing.T) {
	if announcedNextStepWithoutAction("让我们回顾一下改动：") {
		t.Fatal("让我们回顾一下改动 taken for an announced next step")
	}
	if !announcedNextStepWithoutAction("让我修改 main.go。") {
		t.Fatal("让我修改 main.go is still an announcement")
	}
}

// Minor 1: the new completion markers make a short ending an answer.
func TestShortCompletionMarkersAreNotDegenerate(t *testing.T) {
	for _, s := range []string{"已按要求修改。", "已更新。", "已添加测试。", "已删除旧文件。", "已创建。", "搞定。"} {
		if degenerateEnding(s, true) {
			t.Errorf("degenerateEnding(%q) = true after work tools", s)
		}
	}
}

// Minor 2: a failed or blocked call changed nothing and does not count as
// work for degenerateEnding.
func TestNoteWorkToolIgnoresFailedCalls(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil, &mockTool{name: "write", result: "written"})
	for _, res := range []string{"Error: file not found", "BLOCKED: denied by policy"} {
		l := &turnLimits{}
		eng.noteWorkTool(l, "write", res)
		if l.usedTools {
			t.Errorf("result %q counted as work", res)
		}
	}
	l := &turnLimits{}
	eng.noteWorkTool(l, "write", "written")
	if !l.usedTools {
		t.Fatal("a successful write did not count as work")
	}
}

// Minor 2 end to end: a write that failed does not make a short honest
// reply a "degenerate ending" (it used to cost two extra nudged calls).
func TestFailedWriteDoesNotTriggerDegenerateNudge(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("c0", "write", map[string]any{"input": "x"}), nil
		}
		return &api.ChatResponse{Content: "无法写入。"}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) {
		c.DoneCheck = "off"
		c.LoopDetectionDisabled = true
	}, &mockTool{name: "write", result: "Error: permission denied"})
	if _, err := run(t, eng, "改 a.go"); err != nil {
		t.Fatal(err)
	}
	if n := len(prov.requests()); n != 2 {
		t.Fatalf("model calls = %d, want 2", n)
	}
}

// Minor 7: only real engine-injected prefixes mark an old flagless message
// as synthetic; ordinary user text such as "do something" does not.
func TestLooksSyntheticOnlyRealPrefixes(t *testing.T) {
	for _, c := range []string{"do something", "run slow tool", "slow response"} {
		if looksSynthetic(api.Message{Role: "user", Content: c}) {
			t.Errorf("%q taken for an engine-injected message", c)
		}
	}
	if !looksSynthetic(api.Message{Role: "user", Content: "[system: continue]"}) {
		t.Error("[system: prefix not recognised")
	}
}
