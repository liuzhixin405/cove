package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// Steer joins several lines with a newline; TakePendingSteer hands the
// whole batch to the caller and leaves nothing behind. PendingSteer peeks
// without taking and counts the Steer calls, not the lines.
func TestTakePendingSteerReturnsTheBatchAndClearsIt(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.Steer("只看 Go 文件")
	eng.Steer("跳过测试")
	if text, n := eng.PendingSteer(); text != "只看 Go 文件\n跳过测试" || n != 2 {
		t.Fatalf("PendingSteer = %q, %d", text, n)
	}
	if got := eng.TakePendingSteer(); got != "只看 Go 文件\n跳过测试" {
		t.Fatalf("TakePendingSteer = %q", got)
	}
	if text, n := eng.PendingSteer(); text != "" || n != 0 {
		t.Fatalf("after take: PendingSteer = %q, %d", text, n)
	}
	if got := eng.TakePendingSteer(); got != "" {
		t.Fatalf("second take = %q, want empty", got)
	}
}

// A turn that is cancelled before its next model call used to discard the
// guidance typed meanwhile. The engine now leaves it in place so the front
// end can decide where it goes (the REPL runs it as a new task).
func TestCancelledTurnKeepsUnconsumedSteer(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eng.Steer("改用表驱动测试")
	_, err := eng.RunMessageWithStream(ctx, api.Message{Role: "user", Content: "写测试"}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := eng.TakePendingSteer(); got != "改用表驱动测试" {
		t.Fatalf("steer after cancel = %q, want it kept", got)
	}
}

// The same for a turn stopped at its iteration cap: the guidance survives.
func TestLimitStopKeepsUnconsumedSteer(t *testing.T) {
	prov := &seqProvider{reply: func(ctx context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		return toolCallResp("c", "slow", map[string]any{}), nil
	}}
	st := &steeringTool{text: "别再调工具了"}
	eng := newPatternEngine(t, prov, func(c *Config) { c.MaxIterations = 1 }, st)
	st.eng = eng
	if _, err := run(t, eng, "loop"); err == nil {
		t.Fatal("expected the iteration limit to stop the turn")
	}
	if got := eng.TakePendingSteer(); got != "别再调工具了" {
		t.Fatalf("steer after limit stop = %q, want it kept", got)
	}
}

// OnSteerConsumed fires when the loop hands pending guidance to the model,
// so a front end that counts inserted lines can reset its display.
func TestOnSteerConsumedFiresWhenTheModelGetsIt(t *testing.T) {
	prov := &seqProvider{reply: func(ctx context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("s1", "slow", map[string]any{}), nil
		}
		return &api.ChatResponse{Content: "done"}, nil
	}}
	st := &steeringTool{text: "只看 Go 文件"}
	eng := newPatternEngine(t, prov, nil, st)
	st.eng = eng
	consumed := 0
	var leftAtConsume int
	eng.OnSteerConsumed = func() {
		consumed++
		_, leftAtConsume = eng.PendingSteer()
	}
	if _, err := run(t, eng, "list files"); err != nil {
		t.Fatal(err)
	}
	if consumed != 1 {
		t.Fatalf("OnSteerConsumed fired %d times, want 1", consumed)
	}
	if leftAtConsume != 0 {
		t.Fatalf("pending count at consume time = %d, want 0", leftAtConsume)
	}
	found := false
	for _, m := range prov.requests()[1].Messages {
		if m.Role == "user" && strings.Contains(m.Content, "[用户指引] 只看 Go 文件") {
			found = true
		}
	}
	if !found {
		t.Fatal("steer did not reach the model as a [用户指引] message")
	}
}
