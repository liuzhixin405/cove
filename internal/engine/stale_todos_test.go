package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// A task list an earlier request left open does not follow an unrelated
// request: the finish nudge made the new report cover the old task. The
// list is kept (the user may be answering the model's question about it),
// but only a turn that wrote it with todowrite is held to it.
func TestUntouchedTodoListDoesNotForceTheFinishNudge(t *testing.T) {
	isolatedHome(t)
	prov := &mockProvider{responses: []mockResponse{{content: "typo fixed"}}}
	eng := newTestEngine(prov)
	eng.runtime.SetTodos(sampleTodos())

	reply, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "把 README 的错别字改一下"}, nil, nil)
	if err != nil || reply != "typo fixed" {
		t.Fatalf("reply %q, err %v", reply, err)
	}
	if prov.callCount != 1 {
		t.Fatalf("model called %d times: the untouched list sent the new task back to work", prov.callCount)
	}
	for _, m := range eng.messages {
		if strings.Contains(m.Content, "still open") {
			t.Fatalf("finish nudge sent for a list this turn never touched: %q", m.Content)
		}
	}
	if _, open := eng.runtime.TodoList(); open != 2 {
		t.Fatalf("open = %d: the list must be kept, not cleared", open)
	}
}

// Answering the model's question ("A", "用第二种") keeps the plan: the list
// used to be cleared whenever a message lacked a "continue" word.
func TestShortAnswerKeepsThePlan(t *testing.T) {
	isolatedHome(t)
	prov := &mockProvider{responses: []mockResponse{{content: "ok"}, {content: "ok"}}}
	eng := newTestEngine(prov)
	eng.runtime.SetTodos(sampleTodos())
	for _, answer := range []string{"A", "用第二种"} {
		if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: answer}, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, open := eng.runtime.TodoList(); open != 2 {
			t.Fatalf("after %q open = %d, want the list kept", answer, open)
		}
	}
}

// After /resume the plan rebuilt from the history survives the first
// follow-up message.
func TestResumedPlanSurvivesTheFirstFollowUp(t *testing.T) {
	isolatedHome(t)
	prov := &mockProvider{responses: []mockResponse{{content: "ok"}}}
	eng := newTestEngine(prov)
	eng.LoadMessages([]api.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "1", Name: "todowrite", Input: map[string]any{"todos": sampleTodos()}}}},
		{Role: "tool", ToolCallID: "1", Content: "ok"},
		{Role: "assistant", Content: "which option?"},
	})
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "第一种"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, open := eng.runtime.TodoList(); open != 2 {
		t.Fatalf("open = %d after the follow-up, want the resumed plan kept", open)
	}
}

// A turn that wrote the list with todowrite is asked about its open items.
func TestTouchedTodoListGetsTheFinishNudge(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos(sampleTodos())
	tr := &turn{}
	if n := eng.todoFinishNudge(tr); n != "" {
		t.Fatalf("nudge before any todowrite this turn: %q", n)
	}
	eng.todoRoundReminder(tr, []toolResult{{Name: "todowrite"}})
	if n := eng.todoFinishNudge(tr); !strings.Contains(n, "2 item(s)") {
		t.Fatalf("nudge = %q", n)
	}
	// A failed todowrite does not make the list this turn's.
	tr2 := &turn{}
	eng.todoRoundReminder(tr2, []toolResult{{Name: "todowrite", Failed: true}})
	if n := eng.todoFinishNudge(tr2); n != "" {
		t.Fatalf("nudge after a failed todowrite: %q", n)
	}
}

// The round reminder of a list this turn has not touched says it is the
// list of earlier work, to be continued only if the request is part of it.
func TestRoundReminderOfAnEarlierList(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos(sampleTodos())
	read := []toolResult{{Name: "read"}}
	var n string
	tr := &turn{}
	for i := 0; i < todoReminderRounds; i++ {
		n = eng.todoRoundReminder(tr, read)
	}
	if !strings.Contains(n, "from earlier in this conversation") || !strings.Contains(n, "only if the latest request is part of that work") {
		t.Fatalf("untouched reminder = %q", n)
	}
	if strings.Contains(n, "do not stop while items are open") {
		t.Fatalf("untouched reminder holds the model to the old list: %q", n)
	}

	tr = &turn{}
	eng.todoRoundReminder(tr, []toolResult{{Name: "todowrite"}})
	for i := 0; i < todoReminderRounds; i++ {
		n = eng.todoRoundReminder(tr, read)
	}
	if !strings.Contains(n, "Reminder, your task list") {
		t.Fatalf("touched reminder = %q", n)
	}
}

// A resumed turn that wrote the list before the interruption keeps it as
// its own work.
func TestTodoWrittenSince(t *testing.T) {
	msgs := []api.Message{
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "1", Name: "TodoWrite"}}},
	}
	if i := lastRequestIndex(msgs); i != 0 || !todoWrittenSince(msgs[i+1:]) {
		t.Fatalf("index %d, written %v", i, todoWrittenSince(msgs[1:]))
	}
	if todoWrittenSince(msgs[:1]) {
		t.Fatal("no todowrite, yet written")
	}
}

// The final report is about the latest request and states where the changes
// stand in git; the model used to describe the push steps as if run.
func TestReportingBackScopeAndGitState(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	sp := eng.SystemPrompt()
	for _, want := range []string{"latest request only", "committed and pushed", "not committed or not pushed"} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	for name, text := range map[string]string{
		"degenerate": nudgeDegenerateText, "done check": nudgeDoneCheckText, "wrap-up": wrapUpPromptFmt,
	} {
		if !strings.Contains(text, "latest request") {
			t.Errorf("%s prompt is not scoped to the latest request: %q", name, text)
		}
	}
}
