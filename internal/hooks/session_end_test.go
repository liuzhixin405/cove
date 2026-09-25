package hooks

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// At exit a fire-and-forget hook would be killed with the process, so
// FireAndWait runs async hooks too and waits for them.
func TestFireAndWaitWaitsForAsyncHooks(t *testing.T) {
	m := NewManager()
	var done atomic.Int32
	for _, seq := range []bool{true, false} {
		m.Register(HookConfig{Event: SessionEnd, Type: HookRuntime, Sequential: seq,
			RuntimeFn: func(HookInput) (HookOutput, error) {
				time.Sleep(50 * time.Millisecond)
				done.Add(1)
				return HookOutput{Continue: true}, nil
			}})
	}
	m.FireAndWait(context.Background(), SessionEnd, "", HookInput{Event: SessionEnd})
	if got := done.Load(); got != 2 {
		t.Fatalf("%d of 2 hooks had finished when FireAndWait returned", got)
	}
}

// A hook that hangs must not hold the exit up past the caller's deadline.
func TestFireAndWaitStopsAtDeadline(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	m.Register(HookConfig{Event: SessionEnd, Type: HookRuntime, Sequential: true,
		RuntimeFn: func(HookInput) (HookOutput, error) {
			<-release
			return HookOutput{Continue: true}, nil
		}})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	m.FireAndWait(ctx, SessionEnd, "", HookInput{Event: SessionEnd})
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("FireAndWait returned after %v; the deadline was 100ms", el)
	}
}

func TestFireAndWaitHonoursMatcherAndEvent(t *testing.T) {
	m := NewManager()
	var ran atomic.Int32
	fn := func(HookInput) (HookOutput, error) { ran.Add(1); return HookOutput{Continue: true}, nil }
	m.Register(HookConfig{Event: SessionStart, Type: HookRuntime, Sequential: true, RuntimeFn: fn})
	m.Register(HookConfig{Event: SessionEnd, Type: HookRuntime, Sequential: true, Matcher: "^other$", RuntimeFn: fn})
	m.FireAndWait(context.Background(), SessionEnd, "", HookInput{Event: SessionEnd})
	if ran.Load() != 0 {
		t.Fatalf("%d unrelated hooks ran", ran.Load())
	}
}

// Session hooks get the session ID and directory on stdin.
func TestHookInputCarriesSession(t *testing.T) {
	data, err := json.Marshal(HookInput{Event: SessionEnd, SessionID: "abc", Cwd: `D:\proj`})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"session_id":"abc"`, `"cwd":"D:\\proj"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("hook input %s lacks %s", data, want)
		}
	}
	data, _ = json.Marshal(HookInput{Event: BeforeTool, ToolName: "bash"})
	if strings.Contains(string(data), "session_id") {
		t.Errorf("empty session fields are serialized: %s", data)
	}
}
