package covemobile

import (
	"context"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// toolThenAnswer asks for one tool call, then answers; a blocking one waits
// for its context instead of answering.
type toolThenAnswer struct {
	calls   int
	started chan struct{}
	block   bool
}

func (p *toolThenAnswer) Name() string        { return "fake" }
func (p *toolThenAnswer) DisplayName() string { return "fake" }
func (p *toolThenAnswer) Validate() error     { return nil }
func (p *toolThenAnswer) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	return p.ChatStream(ctx, req, nil)
}

func (p *toolThenAnswer) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	p.calls++
	if p.block {
		close(p.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.calls == 1 {
		return &api.ChatResponse{ToolCalls: []api.ToolCall{{ID: "c1", Name: "tap", Input: map[string]any{}}}}, nil
	}
	return &api.ChatResponse{Content: "done"}, nil
}

// resetDuringTool resets the engine while the phone runs the tool, the way
// the app's "new conversation" button can be pressed mid-task.
type resetDuringTool struct {
	recorder
	e *MobileEngine
}

func (r *resetDuringTool) OnToolCall(name, input string) string {
	r.e.Reset()
	return "tapped"
}

// Reset cleared the history but left the running ChatStream going: its tool
// loop then appended the tool result (and the rest of the old turn) onto the
// fresh history, which began with an orphan "tool" message that every later
// request was rejected for.
func TestResetDuringToolCallLeavesHistoryEmpty(t *testing.T) {
	p := &toolThenAnswer{}
	e := newTestEngine(p)
	cb := &resetDuringTool{e: e}
	e.ChatStream("tap it", 30, cb)
	e.mu.Lock()
	msgs := append([]api.Message(nil), e.messages...)
	e.mu.Unlock()
	if len(msgs) != 0 {
		t.Fatalf("history after Reset = %+v, want empty", msgs)
	}
	if p.calls != 1 {
		t.Fatalf("the reset turn went on to %d requests", p.calls)
	}
	if cb.done != "" {
		t.Fatalf("a reset turn reported OnDone(%q)", cb.done)
	}
}

// Reset also stops a request in flight instead of letting it run on.
func TestResetCancelsTheRequestInFlight(t *testing.T) {
	p := &toolThenAnswer{block: true, started: make(chan struct{})}
	e := newTestEngine(p)
	rec := &recorder{}
	done := make(chan struct{})
	go func() { e.ChatStream("hi", 0, rec); close(done) }()
	<-p.started
	e.Reset()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ChatStream kept running after Reset")
	}
	e.mu.Lock()
	n := len(e.messages)
	e.mu.Unlock()
	if n != 0 {
		t.Fatalf("history after Reset has %d messages", n)
	}
}
