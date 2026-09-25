package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
	"github.com/liuzhixin405/cove/internal/tool"
)

// Ctrl+C while the question tool waits on its first of two questions ends
// the tool at once; the second question never takes the input relay, so the
// user's next line is not swallowed as an answer.
func TestInterruptCancelsMultiQuestion(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	rt := &tool.Runtime{AskUser: askUserQuestion}
	done := make(chan tool.Result, 1)
	go func() {
		res, _ := tool.NewQuestionTool().Call(context.Background(), tool.Input{"questions": []any{
			map[string]any{"question": "first?"}, map[string]any{"question": "second?"},
		}}, tool.Context{Runtime: rt})
		done <- res
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "first?") {
		if time.Now().After(deadline) {
			t.Fatal("first question never shown")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !denyPendingPermissionPrompt() {
		t.Fatal("no waiting prompt to interrupt")
	}
	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(res.Data, "cancelled by user") {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the question tool kept waiting after Ctrl+C")
	}
	if ch := repl.TakePermInputCh(); ch != nil {
		t.Fatal("the second question took the input relay")
	}
	if strings.Contains(buf.String(), "second?") {
		t.Fatal("the second question was asked")
	}
}

// The interrupt sentinel is a denial for the permission prompt, never an
// answer that could allow anything.
func TestPermissionPromptTreatsInterruptAsDenial(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })
	done := make(chan bool, 1)
	go func() { done <- askToolPermission(nil, "write", map[string]any{"file_path": "a"}, "") }()
	waitForPermInputCh(t) <- promptInterrupt
	select {
	case allow := <-done:
		if allow {
			t.Fatal("interrupt allowed the call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return")
	}
	if allow, always, persist := permissionAnswerDecision(promptInterrupt); allow || always || persist {
		t.Fatal("sentinel decoded as an answer")
	}
}
