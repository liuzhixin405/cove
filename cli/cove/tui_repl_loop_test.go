package main

import (
	"strings"
	"testing"
	"time"

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
	if msg := enqueueFeedback(0, false, false); msg != "[输入已接收]" {
		t.Errorf("first task feedback = %q", msg)
	}
	if msg := enqueueFeedback(1, false, true); !strings.Contains(msg, "1") {
		t.Errorf("queued feedback = %q does not give the position", msg)
	}
}
