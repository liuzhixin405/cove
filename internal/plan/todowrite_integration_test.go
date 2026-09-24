package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/tool"
)

func todowrite(t *testing.T, rt *tool.Runtime, todos ...map[string]any) string {
	t.Helper()
	items := make([]any, len(todos))
	for i, td := range todos {
		items[i] = td
	}
	res, err := tool.NewTodoWriteTool().Call(context.Background(), tool.Input{"todos": items}, tool.Context{Runtime: rt})
	if err != nil {
		t.Fatalf("todowrite: %v", err)
	}
	return res.Data
}

func todo(content, status string) map[string]any {
	return map[string]any{"content": content, "status": status, "priority": "high"}
}

// The documented workflow is todowrite, then execute_plan. todowrite stored the
// task text in Title and "priority: high" in Description, and the plan reads
// Description, so every sub-agent was sent "priority: high" as its whole task
// and depends: prefixes were never seen.
func TestTodowriteTasksReachThePlanWithTheirTextAndDependencies(t *testing.T) {
	rt := &tool.Runtime{}
	todowrite(t, rt,
		todo("Write the parser", "pending"),
		todo("depends:todo-1 Write the parser tests", "pending"),
	)

	p, err := FromRuntime("plan", rt)
	if err != nil {
		t.Fatalf("FromRuntime: %v", err)
	}
	byID := map[string]*Task{}
	for _, task := range p.Tasks {
		byID[task.ID] = task
	}
	if got := byID["todo-1"]; got == nil || !strings.Contains(got.Description, "Write the parser") {
		t.Fatalf("todo-1 = %+v, want its task text as the description", got)
	}
	if got := byID["todo-2"]; got == nil || len(got.DependsOn) != 1 || got.DependsOn[0] != "todo-1" {
		t.Fatalf("todo-2 = %+v, want a dependency on todo-1", got)
	}
	if byID["todo-2"].Title != "Write the parser tests" {
		t.Fatalf("todo-2 title = %q, want the depends: prefix stripped", byID["todo-2"].Title)
	}
}

// The model has to name task IDs in depends:, so todowrite must tell it what
// the IDs are.
func TestTodowriteOutputShowsTaskIDs(t *testing.T) {
	out := todowrite(t, &tool.Runtime{}, todo("A", "pending"), todo("B", "pending"))
	for _, id := range []string{"todo-1", "todo-2"} {
		if !strings.Contains(out, id) {
			t.Errorf("todowrite output does not mention %s:\n%s", id, out)
		}
	}
}

// Resuming: after the first steps are completed, execute_plan runs again on
// what is left. A dependency on a completed step is satisfied, not unknown.
func TestResumedPlanAcceptsDependenciesOnCompletedTasks(t *testing.T) {
	rt := &tool.Runtime{}
	todowrite(t, rt,
		todo("Write the parser", "completed"),
		todo("depends:todo-1 Write the parser tests", "pending"),
	)
	p, err := FromRuntime("plan", rt)
	if err != nil {
		t.Fatalf("FromRuntime rejected a dependency on a completed task: %v", err)
	}
	if len(p.Tasks) != 1 || p.Tasks[0].ID != "todo-2" {
		t.Fatalf("plan tasks = %+v, want only todo-2", p.Tasks)
	}

	// Execute must run it rather than wait on a dependency outside the plan.
	prov := &scriptedProvider{reply: func(string) (string, error) { return "ok", nil }}
	pe, _ := newExecutor(t, prov)
	pe.runtime = rt
	if res := pe.Execute(context.Background(), p); !res.Success {
		t.Fatalf("Execute failed: %+v", statusByID(res))
	}
}

// A dependency on a task that exists but failed or was cancelled is still an
// error: the plan cannot satisfy it.
func TestPlanRejectsDependencyOnUnfinishedNonPendingTask(t *testing.T) {
	rt := newRuntime(
		pendingTask("b", "depends:a Work"),
		&tool.TaskRecord{ID: "a", Description: "x", Status: "failed"},
	)
	if _, err := FromRuntime("p", rt); err == nil {
		t.Fatal("FromRuntime accepted a dependency on a failed task")
	}
}
