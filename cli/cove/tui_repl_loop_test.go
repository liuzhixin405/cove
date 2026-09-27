package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/repl"
)

// Ctrl+C while the approval box was up cancelled the task's context, but the
// prompt waits on its answer channel, not on the context. So the task stayed
// blocked for up to permissionPromptTimeout (15 minutes), and the next line
// the user typed — a new request — was swallowed as the answer and discarded.
func TestInterruptDeniesAWaitingPermissionPrompt(t *testing.T) {
	buf := captureTurnOutput(t)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh() })

	done := make(chan bool, 1)
	go func() {
		done <- askToolPermission(nil, "bash", map[string]any{"command": "rm -rf build"}, "")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "需要授权") {
		if time.Now().After(deadline) {
			t.Fatal("prompt never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !denyPendingPermissionPrompt() {
		t.Fatal("a waiting prompt was not reported as answered")
	}
	select {
	case allow := <-done:
		if allow {
			t.Fatal("Ctrl+C at the prompt allowed the tool call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt kept waiting after Ctrl+C")
	}
	if ch := repl.TakePermInputCh(); ch != nil {
		t.Fatal("the next typed line would still be taken as a permission answer")
	}
}

func TestDenyPendingPermissionPromptWithoutAPromptIsANoOp(t *testing.T) {
	repl.ClearPermInputCh()
	if denyPendingPermissionPrompt() {
		t.Fatal("reported a prompt answered when none was waiting")
	}
}

// A message typed while a task runs is merged into a task that is still
// queued — never into the one running (Enqueue only looks at the queue). The
// feedback said "已合并进当前处理任务", so the user expected the running task to
// pick the correction up; it only runs after that task finishes.
func TestMergedFeedbackDoesNotClaimTheRunningTask(t *testing.T) {
	for _, idx := range []int{0, 2} {
		msg := enqueueFeedback(idx, true, true)
		if strings.Contains(msg, "当前处理任务") {
			t.Errorf("enqueueFeedback(%d, merged) = %q claims the running task", idx, msg)
		}
		if !strings.Contains(msg, "排队") {
			t.Errorf("enqueueFeedback(%d, merged) = %q does not say it is queued", idx, msg)
		}
	}
	// A message that starts right away needs no line: its output follows.
	if msg := enqueueFeedback(0, false, false); msg != "" {
		t.Errorf("feedback for a task that started at once = %q, want none", msg)
	}
	if msg := enqueueFeedback(1, false, true); !strings.Contains(msg, "1") {
		t.Errorf("queued feedback = %q does not give the position", msg)
	}
	// Typed while a task runs and nothing else is queued: it used to say
	// nothing at all.
	if msg := enqueueFeedback(0, false, true); !strings.Contains(msg, "排队") || !strings.Contains(msg, "结束后") {
		t.Errorf("feedback while running = %q does not say the message is queued for later", msg)
	}
}

// A turn that died of context overflow gets a hint that says what /continue
// will do (compact, then retry) and what to change when that is not enough;
// the generic "/continue 可从中断处继续" left the user guessing whether
// continuing could possibly work.
func TestContextOverflowHintExplainsCompactAndRetry(t *testing.T) {
	err := fmt.Errorf("api: %w", &api.StatusError{Status: 400,
		Msg: `{"error":{"code":400,"message":"request (16569 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error"}}`})
	h := taskErrorHint(err)
	for _, want := range []string{"/continue", "压缩", "上下文长度"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint %q lacks %q", h, want)
		}
	}
	if h := taskErrorHint(fmt.Errorf("api: boom")); h != interruptedTaskHint {
		t.Errorf("other errors keep the generic hint: %q", h)
	}
}

// Enqueue starts an idle runner's message before it returns, so asking
// IsRunning afterwards always said "running" — and the feedback then called
// the very message just typed "queued behind the current task" (on a fresh
// start: "[已排队] 当前任务结束后执行" with nothing else in sight). Whether the
// message started or waits is decided inside Enqueue's lock.
func TestEnqueueWithFeedbackTellsStartedFromQueued(t *testing.T) {
	// Running: the new message waits behind the current task.
	r := newREPLTaskRunner(nil)
	r.mu.Lock()
	r.running = true
	r.current = api.Message{Role: "user", Content: "first"}
	r.mu.Unlock()
	if msg := r.EnqueueWithFeedback(api.Message{Role: "user", Content: "写一个 agent 框架"}); !strings.Contains(msg, "[已排队]") {
		t.Errorf("behind a running task: %q", msg)
	}
	if msg := r.EnqueueWithFeedback(api.Message{Role: "user", Content: "再来一个完全不同的任务 xyz"}); !strings.Contains(msg, "前方排队数: 1") {
		t.Errorf("second in queue: %q", msg)
	}
	if snap := r.Snapshot(); len(snap.Queued) != 2 {
		t.Fatalf("queue = %v", snap.Queued)
	}
	// Idle: the message starts at once and gets no line (enqueueFeedback with
	// wasRunning false, covered above); the decision comes from the state
	// before the start, not from asking IsRunning afterwards.
}
