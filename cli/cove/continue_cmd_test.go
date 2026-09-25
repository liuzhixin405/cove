package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

type fakeInterrupted struct {
	msg api.Message
	ok  bool
}

func (f fakeInterrupted) InterruptedTurn() (api.Message, bool) { return f.msg, f.ok }

func TestContinueResendsTheInterruptedMessage(t *testing.T) {
	var sent []api.Message
	note := continueInterruptedTurn(fakeInterrupted{msg: api.Message{Role: "user", Content: "重构 A"}, ok: true}, false,
		func(m api.Message) { sent = append(sent, m) })
	if len(sent) != 1 || sent[0].Content != "重构 A" {
		t.Fatalf("sent = %+v, want the interrupted message once", sent)
	}
	if !strings.Contains(note, "继续") {
		t.Fatalf("note = %q", note)
	}
}

func TestContinueWithoutInterruptedTurn(t *testing.T) {
	called := false
	note := continueInterruptedTurn(fakeInterrupted{}, false, func(api.Message) { called = true })
	if called || !strings.Contains(note, "没有可继续的回合") {
		t.Fatalf("called=%v note=%q", called, note)
	}
}

func TestContinueWhileRunning(t *testing.T) {
	called := false
	note := continueInterruptedTurn(fakeInterrupted{msg: api.Message{Content: "x"}, ok: true}, true, func(api.Message) { called = true })
	if called || !strings.Contains(note, "正在运行") {
		t.Fatalf("called=%v note=%q", called, note)
	}
}

func TestIsContinueSlashCommand(t *testing.T) {
	for in, want := range map[string]bool{"/continue": true, " /continue ": true, "/continue x": false, "/cont": false, "继续": false} {
		if got := isContinueSlashCommand(in); got != want {
			t.Fatalf("isContinueSlashCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

// The notice after an interrupted task points at /continue, which resumes it.
func TestInterruptedTaskHintPointsAtContinue(t *testing.T) {
	if !strings.Contains(interruptedTaskHint, "/continue") {
		t.Fatalf("hint = %q", interruptedTaskHint)
	}
}
