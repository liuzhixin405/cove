package tool

import (
	"context"
	"strings"
	"testing"
)

func twoQuestions() Input {
	return Input{"questions": []any{
		map[string]any{"header": "a", "question": "first?"},
		map[string]any{"header": "b", "question": "second?"},
	}}
}

// A cancelled first question ends the tool: the second is never asked, and
// the cancellation is not recorded as the user's answer.
func TestQuestionToolStopsOnCancelledAnswer(t *testing.T) {
	asked := 0
	rt := &Runtime{AskUser: func(string) string { asked++; return AskUserCancelled }}
	res, _ := NewQuestionTool().Call(context.Background(), twoQuestions(), Context{Runtime: rt})
	if asked != 1 {
		t.Fatalf("asked %d questions after a cancel", asked)
	}
	if !res.IsError || !strings.Contains(res.Data, "cancelled by user") || strings.Contains(res.Data, "Answer:") {
		t.Fatalf("result = %+v", res)
	}
}

// A cancelled turn asks nothing more.
func TestQuestionToolChecksContext(t *testing.T) {
	asked := 0
	ctx, cancel := context.WithCancel(context.Background())
	rt := &Runtime{AskUser: func(string) string { asked++; cancel(); return "x" }}
	res, _ := NewQuestionTool().Call(ctx, twoQuestions(), Context{Runtime: rt})
	if asked != 1 || !res.IsError || !strings.Contains(res.Data, "cancelled by user") {
		t.Fatalf("asked=%d result=%+v", asked, res)
	}
}
