package repl

import "testing"

// A prompt registers what counts as an answer. A line that is one is handed
// to the prompt; any other line leaves the prompt waiting and is the
// caller's to dispatch, with the prompt's hint to show. Type-ahead for the
// next task used to be swallowed as a "no".
func TestTakePromptInputForDistinguishesAnswers(t *testing.T) {
	t.Cleanup(ClearPermInputCh)
	ch := make(chan string, 1)
	yesNo := func(s string) bool { return s == "y" || s == "n" }
	SetPromptInput(ch, yesNo, "请输入 y 或 n")

	got, state, hint := TakePromptInputFor("请把测试也改掉")
	if state != PromptNotAnswer || got != nil || hint != "请输入 y 或 n" {
		t.Fatalf("non-answer: ch=%v state=%v hint=%q", got, state, hint)
	}
	if _, state, _ = TakePromptInputFor(""); state != PromptNotAnswer {
		t.Fatalf("empty line: state=%v, want not-answer", state)
	}
	got, state, _ = TakePromptInputFor("y")
	if state != PromptAnswer || got == nil {
		t.Fatalf("answer: ch=%v state=%v", got, state)
	}
	if _, state, _ = TakePromptInputFor("y"); state != PromptNone {
		t.Fatalf("after the answer the prompt must be gone: %v", state)
	}

	// A prompt that takes any text (the question tool) still refuses an
	// empty line, and Ctrl+C's TakePermInputCh takes the channel whatever
	// the line would have been.
	SetPromptInput(ch, nil, "请输入回答")
	if _, state, _ := TakePromptInputFor(""); state != PromptNotAnswer {
		t.Fatalf("empty line for a free-text prompt: %v", state)
	}
	if _, state, _ := TakePromptInputFor("anything"); state != PromptAnswer {
		t.Fatalf("free text not accepted: %v", state)
	}
	SetPromptInput(ch, yesNo, "")
	if TakePermInputCh() == nil {
		t.Fatal("TakePermInputCh must take the channel unconditionally")
	}
}

// ⚡ (U+26A1) is East Asian Wide: measured as one cell the running prompt
// was one column short and the row could touch the last column.
func TestLightningIsTwoCellsWide(t *testing.T) {
	if w := runeCellWidth('⚡'); w != 2 {
		t.Fatalf("width(⚡) = %d, want 2", w)
	}
}
