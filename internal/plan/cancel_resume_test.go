package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Ctrl+C mid-plan used to mark the interrupted task "failed" and everything
// after it "failed"/"skipped" in the runtime, and execute_plan only picks up
// "pending" tasks, so after an interrupt there was nothing left to resume:
// "continue" answered "no pending tasks found". Interrupted and unstarted
// tasks must go back to pending, and the summary must say they were cancelled.
func TestCancelledPlanLeavesUnfinishedTasksPendingForResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		if strings.HasPrefix(prompt, "first") {
			return "done first", nil
		}
		close(started)
		<-ctx.Done()
		return "", errors.New("context canceled")
	}

	pe, rt := newExecutor(t, p, "a", "b", "c")
	pe.SetMaxRetries(2)
	for id, desc := range map[string]string{"a": "first", "b": "depends:a second", "c": "depends:b third"} {
		rt.Tasks[id].Description = desc
	}
	plan := &Plan{ID: "p", Tasks: []*Task{
		{ID: "a", Description: "first", Status: "pending"},
		{ID: "b", Description: "second", Status: "pending", DependsOn: []string{"a"}},
		{ID: "c", Description: "third", Status: "pending", DependsOn: []string{"b"}},
	}}

	done := make(chan *ExecutionResult, 1)
	go func() { done <- pe.Execute(ctx, plan) }()
	<-started
	cancel()
	res := <-done

	got := statusByID(res)
	if got["a"] != "done" {
		t.Errorf("a = %q, want done (it finished before the interrupt)", got["a"])
	}
	for _, id := range []string{"b", "c"} {
		if got[id] != "cancelled" {
			t.Errorf("%s = %q in the result, want cancelled", id, got[id])
		}
	}
	rt.Lock()
	for id, want := range map[string]string{"a": "done", "b": "pending", "c": "pending"} {
		if rt.Tasks[id].Status != want {
			t.Errorf("runtime %s = %q, want %q", id, rt.Tasks[id].Status, want)
		}
	}
	rt.Unlock()

	if out := FormatResult(res); !strings.Contains(out, "cancelled") {
		t.Errorf("summary does not say the run was cancelled:\n%s", out)
	}

	// The interrupted task is not retried against a cancelled context.
	calls := 0
	for _, pr := range p.seenPrompts() {
		if strings.HasPrefix(pr, "second") {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("interrupted task was dispatched %d times, want 1", calls)
	}

	// And a later execute_plan picks the unfinished work back up.
	resumed, err := FromRuntime("p", rt)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resumed.Tasks) != 2 {
		t.Fatalf("resumed plan has %d tasks, want b and c", len(resumed.Tasks))
	}
}
