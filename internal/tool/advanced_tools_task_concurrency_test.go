package tool

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The task tools are all declared IsConcurrencySafe, so the engine dispatches
// every one of them into its own goroutine when a single model response asks
// for more than one tool call (engine.go parallel tool dispatch). That makes
// Runtime.Tasks a concurrently-accessed map: task_create/task_update/task_stop
// write it, task_list/task_get/task_output/brief plus plan.FromRuntime read it.
//
// Tools that are NOT concurrency-safe are not off the hook either: the engine
// runs those inline in the same dispatch loop, so they execute while the
// goroutines already launched in that loop are still in flight. todowrite and
// team_create both mutate Tasks that way.
//
// A concurrent map read and write is not a recoverable panic — Go throws
// "fatal error: concurrent map read and map write", which kills the process
// mid-turn. Every reader must therefore take Runtime's lock.
//
// Run with -race: the detector reports the unsynchronized pairs directly.
func TestTaskToolsAreSafeToCallConcurrently(t *testing.T) {
	rt := &Runtime{
		Tasks: map[string]*TaskRecord{
			"task-0": {ID: "task-0", Title: "seed", Status: "pending"},
		},
	}
	tctx := Context{Runtime: rt}
	ctx := context.Background()

	create := NewTaskCreateTool()
	list := NewTaskListTool()
	get := NewTaskGetTool()
	output := NewTaskOutputTool()
	update := NewTaskUpdateTool()
	brief := NewBriefTool()
	todos := NewTodoWriteTool()
	team := NewTeamCreateTool()

	const rounds = 40
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(8)
		go func() {
			defer wg.Done()
			_, _ = create.Call(ctx, Input{"title": "t", "description": "d"}, tctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = list.Call(ctx, Input{}, tctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = get.Call(ctx, Input{"taskId": "task-0"}, tctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = output.Call(ctx, Input{"taskId": "task-0"}, tctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = update.Call(ctx, Input{"taskId": "task-0", "status": "running"}, tctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = brief.Call(ctx, Input{}, tctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = todos.Call(ctx, Input{"todos": []any{
				map[string]any{"content": "x", "status": "pending", "priority": "high"},
			}}, tctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = team.Call(ctx, Input{"name": "t", "members": []any{
				map[string]any{"agent": "a", "task": "b"},
			}}, tctx)
		}()
	}
	wg.Wait()
}

// The test above proves the race is real, but it only fires on some runs — it
// passed on one of the two runs that exposed it. That makes it a detector, not
// a regression guard.
//
// This test pins the contract deterministically instead: while the test holds
// Runtime's lock, no tool that touches the task maps may complete. Every one of
// these calls is pure in-memory map work measured in microseconds, so a tool
// that skips the lock returns long before the 100ms window closes and the
// assertion fails on every run. A tool that takes the lock blocks on it, which
// is the behavior we want.
func TestTaskToolsTakeTheRuntimeLock(t *testing.T) {
	rt := &Runtime{Tasks: map[string]*TaskRecord{
		"task-0": {ID: "task-0", Title: "seed", Status: "pending"},
	}}
	tctx := Context{Runtime: rt}
	ctx := context.Background()

	tools := []struct {
		name string
		call func()
	}{
		{"task_list", func() { _, _ = NewTaskListTool().Call(ctx, Input{}, tctx) }},
		{"task_get", func() { _, _ = NewTaskGetTool().Call(ctx, Input{"taskId": "task-0"}, tctx) }},
		{"task_output", func() { _, _ = NewTaskOutputTool().Call(ctx, Input{"taskId": "task-0"}, tctx) }},
		{"brief", func() { _, _ = NewBriefTool().Call(ctx, Input{}, tctx) }},
		{"todowrite", func() {
			_, _ = NewTodoWriteTool().Call(ctx, Input{"todos": []any{
				map[string]any{"content": "x", "status": "pending", "priority": "high"},
			}}, tctx)
		}},
		{"team_create", func() {
			_, _ = NewTeamCreateTool().Call(ctx, Input{"name": "t", "members": []any{
				map[string]any{"agent": "a", "task": "b"},
			}}, tctx)
		}},
	}

	for _, tc := range tools {
		t.Run(tc.name, func(t *testing.T) {
			rt.Lock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.call()
			}()

			select {
			case <-done:
				t.Errorf("%s completed while Runtime was locked: it never took the lock", tc.name)
			case <-time.After(100 * time.Millisecond):
			}

			rt.Unlock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s never finished after the lock was released (deadlock?)", tc.name)
			}
		})
	}
}

// A reader must also observe the record fields under the lock: task_update
// rewrites Status/Output on the pointed-to record while task_list/task_get
// read them.
func TestTaskListSeesUpdatesUnderLock(t *testing.T) {
	rt := &Runtime{
		Tasks: map[string]*TaskRecord{
			"task-0": {ID: "task-0", Title: "seed", Status: "pending"},
		},
	}
	tctx := Context{Runtime: rt}
	ctx := context.Background()

	update := NewTaskUpdateTool()
	list := NewTaskListTool()

	if _, err := update.Call(ctx, Input{"taskId": "task-0", "status": "running"}, tctx); err != nil {
		t.Fatalf("update: %v", err)
	}
	res, err := list.Call(ctx, Input{}, tctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := "running"; !strings.Contains(res.Data, want) {
		t.Errorf("task_list output should reflect the status written by task_update, got %q", res.Data)
	}
}
