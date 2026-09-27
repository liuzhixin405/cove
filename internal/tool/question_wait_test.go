package tool

import (
	"context"
	"testing"
)

// While the question tool waits for the person's answer the stall monitor
// must know the tool is waiting, not stuck: it tells the engine through
// Context.SetWaiting around AskUser, as the permission prompt does.
func TestQuestionToolMarksItselfWaitingWhileAsking(t *testing.T) {
	var waits []bool
	rt := &Runtime{AskUser: func(prompt string) string {
		if len(waits) != 1 || !waits[0] {
			t.Errorf("AskUser called before SetWaiting(true): %v", waits)
		}
		return "1"
	}}
	tctx := Context{Runtime: rt, SetWaiting: func(w bool) { waits = append(waits, w) }}
	input := Input{"questions": []any{map[string]any{
		"header": "h", "question": "q",
		"options": []any{map[string]any{"label": "yes", "description": "d"}},
	}}}
	if _, err := NewQuestionTool().Call(context.Background(), input, tctx); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || !waits[0] || waits[1] {
		t.Fatalf("SetWaiting calls = %v, want [true false]", waits)
	}
}
