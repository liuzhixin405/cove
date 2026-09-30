package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// A panic on the turn goroutine left no interruption marker: the REPL's
// recover offered "继续", beginTurn took the re-sent message for a new
// request and appended it a second time, so the model saw the request twice
// and redid the completed steps. The panic itself still reaches the caller.
func TestPanicDuringTurnLeavesAResumableInterruption(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{{content: "恢复后完成"}}}
	eng := newTestEngine(prov)
	eng.Steer("顺便看看日志")
	eng.OnSteerConsumed = func() { panic("injected by the test") }
	msg := api.Message{Role: "user", Content: "做一件会出异常的事"}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_, _ = eng.RunMessageWithStream(context.Background(), msg, nil, nil)
	}()
	if !eng.HasInterruptedTurn() {
		t.Fatal("no resumable interruption after the panic")
	}

	eng.OnSteerConsumed = nil
	if _, err := eng.RunMessageWithStream(context.Background(), msg, nil, nil); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range prov.lastReq.Messages {
		if m.Role == "user" && m.Content == msg.Content {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the request appears %d times in the resumed turn, want once", n)
	}
	if eng.HasInterruptedTurn() {
		t.Fatal("the resumed turn completed but is still marked interrupted")
	}
}
