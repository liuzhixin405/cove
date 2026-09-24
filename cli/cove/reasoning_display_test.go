package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/termui"
)

// reasoningRunner streams a long reasoning trace and then a short answer, the
// way a thinking model does.
type reasoningRunner struct{}

func (reasoningRunner) RunWithStream(ctx context.Context, input string, onDelta func(string)) (string, error) {
	return reasoningRunner{}.RunMessageWithStream(ctx, api.Message{Content: input}, onDelta, nil)
}

func (reasoningRunner) RunMessageWithStream(ctx context.Context, msg api.Message, onDelta func(string), onReasoning func(string)) (string, error) {
	if onReasoning != nil {
		onReasoning("RAW-REASONING: first I will consider every file in the repo, then ")
		onReasoning("RAW-REASONING: weigh each option in detail")
	}
	onDelta("the answer")
	return "the answer", nil
}

func captureTerminal(t *testing.T, show bool) string {
	t.Helper()
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	t.Cleanup(func() { termui.SetWriter(nil) })
	old := showReasoning
	showReasoning = show
	t.Cleanup(func() { showReasoning = old })

	if _, err := runChatInteractionMessage(context.Background(), reasoningRunner{}, api.Message{Role: "user", Content: "q"}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// A thinking model's full reasoning was streamed into the conversation, so the
// user scrolled through pages of scratch work to find the answer. By default it
// is summarised in the status line; show_reasoning restores the full stream.
func TestReasoningIsFoldedByDefault(t *testing.T) {
	out := captureTerminal(t, false)
	if strings.Contains(out, "RAW-REASONING") {
		t.Fatal("raw reasoning printed into the conversation")
	}
	if !strings.Contains(out, "the answer") {
		t.Fatal("the answer itself is missing")
	}
}

func TestShowReasoningPrintsTheFullTrace(t *testing.T) {
	if out := captureTerminal(t, true); !strings.Contains(out, "RAW-REASONING") {
		t.Fatal("show_reasoning did not print the reasoning")
	}
}
