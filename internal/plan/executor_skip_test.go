package plan

import "testing"

// TestBlockingDep covers the dependency gate that Execute uses to decide
// whether a task may run. The regression it guards: a task whose dependency
// failed used to be marked "skipped" and then executed anyway, because runTask
// reset Status to "running" unconditionally.
func TestBlockingDep(t *testing.T) {
	tasks := map[string]*Task{
		"a": {ID: "a", Status: "done"},
		"b": {ID: "b", Status: "failed"},
		"c": {ID: "c", Status: "skipped"},
	}

	cases := []struct {
		name string
		task *Task
		want string
	}{
		{"no deps", &Task{ID: "x"}, ""},
		{"dep succeeded", &Task{ID: "x", DependsOn: []string{"a"}}, ""},
		{"dep failed", &Task{ID: "x", DependsOn: []string{"b"}}, "b"},
		{"dep skipped propagates", &Task{ID: "x", DependsOn: []string{"c"}}, "c"},
		{"first bad dep wins", &Task{ID: "x", DependsOn: []string{"a", "b", "c"}}, "b"},
		{"unknown dep is not blocking", &Task{ID: "x", DependsOn: []string{"nope"}}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockingDep(tc.task, tasks); got != tc.want {
				t.Fatalf("blockingDep = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunTaskRefusesSkipped verifies the backstop inside runTask: a task
// already ruled out must not be revived into "running".
func TestRunTaskRefusesSkipped(t *testing.T) {
	pe := &PlanExecutor{}
	task := &Task{ID: "x", Status: "skipped"}

	if pe.runTask(t.Context(), task, map[string]bool{}) {
		t.Fatal("runTask reported success for a skipped task")
	}
	if task.Status != "skipped" {
		t.Fatalf("runTask changed status to %q, want it left as skipped", task.Status)
	}
}
