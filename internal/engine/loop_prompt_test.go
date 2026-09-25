package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// loopingEngine repeats the same mutating call until finishAfter requests
// (0 = never), with Layer 1 tuned to fire on every second repeat.
func loopingEngine(t *testing.T, finishAfter int) (*Engine, *seqProvider) {
	t.Helper()
	prov := &seqProvider{reply: func(_ context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if len(req.Tools) == 0 {
			return &api.ChatResponse{Content: "总结：重复调用了 mut_tool。"}, nil
		}
		if finishAfter > 0 && n >= finishAfter {
			return &api.ChatResponse{Content: "完成"}, nil
		}
		return toolCallResp(fmt.Sprintf("c%d", n), "mut_tool", map[string]any{"input": "same"}), nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) { c.DoneCheck = "off" }, &mockTool{name: "mut_tool", result: "ok"})
	eng.loopDetector.fpThresh = 2
	eng.loopDetector.toolOnlyThresh = 100
	eng.loopDetector.outThresh = 100
	return eng, prov
}

func TestLoopSecondHitAsksAndStops(t *testing.T) {
	eng, prov := loopingEngine(t, 0)
	rec := &limitRecorder{}
	eng.IterationLimitPrompt = rec.prompt
	_, err := run(t, eng, "做")
	var le *LimitError
	if !errors.As(err, &le) || le.Reason != LimitReasonLoop {
		t.Fatalf("err = %#v, want a loop LimitError", err)
	}
	if len(rec.calls) != 1 || rec.calls[0].Reason != LimitReasonLoop || rec.calls[0].Detail == "" {
		t.Fatalf("prompt calls = %+v, want one with Reason loop and the detector's reason", rec.calls)
	}
	// Hits at the 2nd and 4th repeat: the prompt came at the second one.
	if n := len(toolRequests(prov)); n != 4 {
		t.Fatalf("model called %d times before the prompt, want 4", n)
	}
	if last := eng.messages[len(eng.messages)-1]; last.Role != "assistant" || last.Content != "总结：重复调用了 mut_tool。" {
		t.Fatalf("a loop stop must end with the wrap-up summary, last = %+v", last)
	}
}

func TestLoopSecondHitContinueDisablesDetection(t *testing.T) {
	eng, prov := loopingEngine(t, 12)
	rec := &limitRecorder{decisions: []LimitDecision{LimitContinue}}
	eng.IterationLimitPrompt = rec.prompt
	reply, err := run(t, eng, "做")
	if err != nil || reply != "完成" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("prompt called %d times, want 1: detection is off for the rest of the turn", len(rec.calls))
	}
	if n := len(prov.requests()); n != 13 {
		t.Fatalf("model called %d times, want 13", n)
	}
	if got := countMsgs(eng.messages, loopAbortToolNote); got != 1 {
		t.Fatalf("batches skipped by loop detection = %d, want 1 (the first hit only)", got)
	}
}

func TestLoopWithoutPromptKeepsHardStop(t *testing.T) {
	eng, _ := loopingEngine(t, 0)
	_, err := run(t, eng, "做")
	if err == nil {
		t.Fatal("want the fatal loop stop")
	}
	var le *LimitError
	if errors.As(err, &le) {
		t.Fatalf("err = %v: without a prompt the loop stop is unchanged", err)
	}
}

func TestLoopDetectorDisableForTurn(t *testing.T) {
	ld := NewLoopDetector()
	ld.fpThresh = 2
	ld.outThresh = 2
	ld.DisableForTurn()
	for i := 0; i < 5; i++ {
		if lr := ld.RecordToolCalls("mut_tool:same"); lr.Detected {
			t.Fatal("a disabled detector reported a loop")
		}
		if lr := ld.RecordOutput("same output"); lr.Detected {
			t.Fatal("a disabled detector reported an output loop")
		}
	}
	ld.ResetTurn()
	ld.RecordToolCalls("mut_tool:same")
	if lr := ld.RecordToolCalls("mut_tool:same"); !lr.Detected {
		t.Fatal("ResetTurn must re-enable detection")
	}
}
