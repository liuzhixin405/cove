package tool

import (
	"context"
	"strings"
	"testing"
)

// task_stop answered "Task X stopped." with IsError=false for an id it had
// never seen, so the model believed a mistyped id had stopped a task. It
// reports not found like task_get and task_update.
func TestTaskStopUnknownTaskIsAnError(t *testing.T) {
	rt := &Runtime{Tasks: map[string]*TaskRecord{
		"task-0": {ID: "task-0", Title: "seed", Status: "running"},
	}}
	stop := NewTaskStopTool()
	for _, tctx := range []Context{{Runtime: rt}, {}} {
		res, err := stop.Call(context.Background(), Input{"taskId": "task-9"}, tctx)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Data, "not found") {
			t.Errorf("unknown task (runtime=%v): %+v, want a not-found error", tctx.Runtime != nil, res)
		}
	}
	res, err := stop.Call(context.Background(), Input{"taskId": "task-0"}, Context{Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || rt.Tasks["task-0"].Status != "cancelled" {
		t.Errorf("known task: %+v, status %q", res, rt.Tasks["task-0"].Status)
	}
}
