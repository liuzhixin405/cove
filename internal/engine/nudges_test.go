package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

func TestAnnouncedNextStepWithoutAction(t *testing.T) {
	yes := []string{
		"接下来我将修改 main.go。",
		"Let me now update the config.",
		"Next, I'll run the tests",
		"我已经读完了代码结构。\n\n下一步：实现解析函数",
	}
	no := []string{
		"已完成全部修改。",
		"需要我继续吗？",
		"The task is done.",
		"你希望用哪种方案？",
		"",
		"Let me know if you need anything else.",
		"下一步建议：运行 go test ./... 确认。",
		// Review round 1: handing over, waiting, negation, word boundaries.
		"Once you confirm, I'll proceed with the migration.",
		"The fix handles the next iteration correctly.",
		"Here is the answer: 42. Next item in the list is unchanged.",
		"You can now run make; I will not touch the Makefile.",
		"I'm going to stop here because the remaining work needs your credentials.",
		"Summary: updated config.\n\nI'll leave the rest to you.",
		"已修复 bug。\n\n下一步：请你运行 go test ./... 验证。",
		"下一步（需要你操作）：重启服务",
		"改动如上。如有问题我将继续跟进。",
		"修改已提交。之后我将根据你的反馈再调整。",
	}
	for _, s := range yes {
		if !announcedNextStepWithoutAction(s) {
			t.Errorf("announcedNextStepWithoutAction(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if announcedNextStepWithoutAction(s) {
			t.Errorf("announcedNextStepWithoutAction(%q) = true, want false", s)
		}
	}
}

func TestDegenerateEnding(t *testing.T) {
	cases := []struct {
		content string
		used    bool
		want    bool
	}{
		{"好的。", true, true},
		{"ok", true, true},
		{"已完成。", true, false},
		{"All done.", true, false},
		{"Finished", true, false},
		{"好的。", false, false},
		{"这是一段足够长的最终答复，说明了修改了哪些文件、为什么这样改、以及还剩下什么需要用户确认的事项。", true, false},
		// CJK text carries more per character: 20 is the bar there.
		{"改了两处调用方并更新了注释说明内容", true, true},
		{"改了两处调用方，并同步更新了相关注释和测试用例", true, false},
	}
	for _, c := range cases {
		if got := degenerateEnding(c.content, c.used); got != c.want {
			t.Errorf("degenerateEnding(%q, %v) = %v, want %v", c.content, c.used, got, c.want)
		}
	}
}

// A short answer after only reading is an answer, not a fragment.
func TestDegenerateEndingIgnoresReadOnlyTurns(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("c0", "read_tool", map[string]any{"query": "a.go"}), nil
		}
		return &api.ChatResponse{Content: "文件 a.go 第 12 行是 return nil。"}, nil
	}}
	eng := nudgeEngine(t, prov)
	reply, err := run(t, eng, "a.go 第 12 行是什么")
	if err != nil || reply != "文件 a.go 第 12 行是 return nil。" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if n := len(prov.requests()); n != 2 {
		t.Fatalf("model called %d times, want 2: a read-only turn gets no degenerate-ending nudge", n)
	}
}

func TestEmptyOrThinkOnly(t *testing.T) {
	cases := []struct {
		name string
		resp *api.ChatResponse
		want bool
	}{
		{"empty", &api.ChatResponse{}, true},
		{"blank", &api.ChatResponse{Content: "  \n"}, true},
		{"reasoning only", &api.ChatResponse{ReasoningContent: "thinking..."}, true},
		{"thinking blocks only", &api.ChatResponse{ThinkingBlocks: []json.RawMessage{json.RawMessage(`{"type":"thinking"}`)}}, true},
		{"text", &api.ChatResponse{Content: "答案"}, false},
		{"tool call", &api.ChatResponse{ToolCalls: []api.ToolCall{{ID: "1", Name: "read"}}}, false},
	}
	for _, c := range cases {
		if got := emptyOrThinkOnly(c.resp); got != c.want {
			t.Errorf("%s: emptyOrThinkOnly = %v, want %v", c.name, got, c.want)
		}
	}
}
