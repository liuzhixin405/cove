package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

func nudgeEngine(t *testing.T, prov *seqProvider) *Engine {
	t.Helper()
	return newPatternEngine(t, prov, func(c *Config) { c.LoopDetectionDisabled = true; c.DoneCheck = "off" },
		&mockTool{name: "read_tool", readOnly: true, safe: true, result: "ok"},
		&mockTool{name: "mut_tool", result: "ok"})
}

func countMsgs(msgs []api.Message, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			n++
		}
	}
	return n
}

func TestNudgeAnnouncedNextStepContinuesUpToCap(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "接下来我将修改 X"}, nil
	}}
	eng := nudgeEngine(t, prov)
	reply, err := run(t, eng, "改一下 X")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	reqs := prov.requests()
	if len(reqs) != 1+maxContinuationNudgesPerTurn {
		t.Fatalf("model called %d times, want %d", len(reqs), 1+maxContinuationNudgesPerTurn)
	}
	if reply != "接下来我将修改 X" {
		t.Fatalf("reply = %q", reply)
	}
	second := reqs[1].Messages
	last := second[len(second)-1]
	if last.Role != "user" || !last.Synthetic || last.Content != nudgeAnnouncedText {
		t.Fatalf("second request ends with %+v, want the announced-step nudge", last)
	}
	if got := countMsgs(eng.messages, nudgeAnnouncedText); got != maxContinuationNudgesPerTurn {
		t.Fatalf("history holds %d nudges, want %d", got, maxContinuationNudgesPerTurn)
	}
}

func TestNudgeAnnouncedThenActs(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		switch n {
		case 0:
			return &api.ChatResponse{Content: "Let me now update the config."}, nil
		case 1:
			return toolCallResp("c1", "read_tool", map[string]any{"query": "x"}), nil
		default:
			return &api.ChatResponse{Content: "The task is done: config updated and verified."}, nil
		}
	}}
	eng := nudgeEngine(t, prov)
	reply, err := run(t, eng, "update config")
	if err != nil || !strings.Contains(reply, "done") {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if n := len(prov.requests()); n != 3 {
		t.Fatalf("model called %d times, want 3", n)
	}
}

func TestNudgeDegenerateEndingAfterTools(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("c0", "mut_tool", map[string]any{"input": "x"}), nil
		}
		return &api.ChatResponse{Content: "好的。"}, nil
	}}
	eng := nudgeEngine(t, prov)
	if _, err := run(t, eng, "看看"); err != nil {
		t.Fatal(err)
	}
	if n := len(prov.requests()); n != 2+maxContinuationNudgesPerTurn {
		t.Fatalf("model called %d times, want %d", n, 2+maxContinuationNudgesPerTurn)
	}
	if got := countMsgs(eng.messages, nudgeDegenerateText); got != maxContinuationNudgesPerTurn {
		t.Fatalf("degenerate nudges = %d", got)
	}
}

func TestNudgeContinuationCapIsShared(t *testing.T) {
	// An announced step, then a degenerate ending: both draw from one quota.
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		switch n {
		case 0:
			return toolCallResp("c0", "mut_tool", map[string]any{"input": "x"}), nil
		case 1:
			return &api.ChatResponse{Content: "接下来我将修改 X"}, nil
		default:
			return &api.ChatResponse{Content: "好的。"}, nil
		}
	}}
	eng := nudgeEngine(t, prov)
	if _, err := run(t, eng, "改"); err != nil {
		t.Fatal(err)
	}
	if n := len(prov.requests()); n != 2+maxContinuationNudgesPerTurn {
		t.Fatalf("model called %d times, want %d", n, 2+maxContinuationNudgesPerTurn)
	}
}

func TestNudgeEmptyResponseRetriesUpToCap(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{ReasoningContent: "thinking only"}, nil
	}}
	eng := nudgeEngine(t, prov)
	if _, err := run(t, eng, "问题"); err != nil {
		t.Fatal(err)
	}
	reqs := prov.requests()
	if len(reqs) != 1+maxEmptyResponseRetries {
		t.Fatalf("model called %d times, want %d", len(reqs), 1+maxEmptyResponseRetries)
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if last.Content != nudgeEmptyText {
		t.Fatalf("retry request ends with %q, want the empty-response nudge", last.Content)
	}
	// An empty assistant turn is never written into history.
	for _, m := range eng.messages[:len(eng.messages)-1] {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("empty assistant message kept in history: %+v", m)
		}
	}
}

func TestNudgeEmptyThenAnswer(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return &api.ChatResponse{}, nil
		}
		return &api.ChatResponse{Content: "答案是 42，已完成。"}, nil
	}}
	eng := nudgeEngine(t, prov)
	reply, err := run(t, eng, "问题")
	if err != nil || reply != "答案是 42，已完成。" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
}

func TestNudgeSkippedWhenOverBudget(t *testing.T) {
	var eng *Engine
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		exhaustBudget(eng)
		return &api.ChatResponse{Content: "接下来我将修改 X"}, nil
	}}
	eng = nudgeEngine(t, prov)
	reply, err := run(t, eng, "改一下 X")
	if err != nil || reply != "接下来我将修改 X" {
		t.Fatalf("reply=%q err=%v: a nudge must not push a turn over its budget", reply, err)
	}
	if n := len(prov.requests()); n != 1 {
		t.Fatalf("model called %d times, want 1", n)
	}
}

// exhaustBudget spends the engine's whole budget.
func exhaustBudget(eng *Engine) {
	eng.costTracker.SetMaxBudget(0.0001)
	eng.costTracker.Add("claude-sonnet-4-20250514", 1_000_000, 1_000_000)
}
