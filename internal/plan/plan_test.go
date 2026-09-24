package plan

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/tool"
)

// newRuntime builds a tool.Runtime holding the given tasks.
func newRuntime(tasks ...*tool.TaskRecord) *tool.Runtime {
	rt := &tool.Runtime{Tasks: make(map[string]*tool.TaskRecord, len(tasks))}
	for _, t := range tasks {
		rt.Tasks[t.ID] = t
	}
	return rt
}

func pendingTask(id, desc string) *tool.TaskRecord {
	return &tool.TaskRecord{ID: id, Description: desc, Status: "pending"}
}

// ---------- FromRuntime ----------

func TestFromRuntimeParsesDependencies(t *testing.T) {
	rt := newRuntime(
		pendingTask("a", "Write the parser"),
		pendingTask("b", "depends:a Write the tests"),
		pendingTask("c", "depends:a,b Wire it up"),
	)

	p, err := FromRuntime("plan-1", rt)
	if err != nil {
		t.Fatalf("FromRuntime: %v", err)
	}
	if p.ID != "plan-1" {
		t.Errorf("PlanID = %q, want %q", p.ID, "plan-1")
	}
	if len(p.Tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(p.Tasks))
	}

	byID := map[string]*Task{}
	for _, task := range p.Tasks {
		byID[task.ID] = task
	}

	if got := byID["a"].DependsOn; len(got) != 0 {
		t.Errorf("a.DependsOn = %v, want empty", got)
	}
	if got := byID["b"].DependsOn; len(got) != 1 || got[0] != "a" {
		t.Errorf("b.DependsOn = %v, want [a]", got)
	}
	if got := byID["c"].DependsOn; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("c.DependsOn = %v, want [a b]", got)
	}

	// The "depends:" prefix must be stripped from the display title but kept in
	// Description, which is what is actually sent to the sub-agent as the task.
	if byID["b"].Title != "Write the tests" {
		t.Errorf("b.Title = %q, want the prefix stripped", byID["b"].Title)
	}
	if !strings.HasPrefix(byID["b"].Description, DepPrefix) {
		t.Errorf("b.Description = %q, want the original content preserved", byID["b"].Description)
	}
}

func TestFromRuntimeSkipsNonPendingAndEmpty(t *testing.T) {
	rt := newRuntime(
		pendingTask("a", "Do the thing"),
		&tool.TaskRecord{ID: "done", Description: "Already finished", Status: "done"},
		&tool.TaskRecord{ID: "running", Description: "In flight", Status: "running"},
		pendingTask("blank", "   "), // whitespace-only content is not a task
	)

	p, err := FromRuntime("p", rt)
	if err != nil {
		t.Fatalf("FromRuntime: %v", err)
	}
	if len(p.Tasks) != 1 || p.Tasks[0].ID != "a" {
		ids := []string{}
		for _, task := range p.Tasks {
			ids = append(ids, task.ID)
		}
		t.Fatalf("tasks = %v, want only [a]", ids)
	}
}

func TestFromRuntimeErrors(t *testing.T) {
	t.Run("no tasks at all", func(t *testing.T) {
		if _, err := FromRuntime("p", &tool.Runtime{}); err == nil {
			t.Fatal("expected an error for an empty runtime")
		}
	})

	t.Run("no pending tasks", func(t *testing.T) {
		rt := newRuntime(&tool.TaskRecord{ID: "a", Description: "x", Status: "done"})
		_, err := FromRuntime("p", rt)
		if err == nil || !strings.Contains(err.Error(), "no pending tasks") {
			t.Fatalf("err = %v, want a no-pending-tasks error", err)
		}
	})

	t.Run("dependency on an unknown task", func(t *testing.T) {
		// This used to use a "done" task as the ghost, pinning that a
		// dependency on finished work is rejected — which broke resuming a
		// plan. A finished dependency is satisfied now (see
		// TestResumedPlanAcceptsDependenciesOnCompletedTasks); only a task
		// that does not exist at all is unknown.
		rt := newRuntime(
			pendingTask("b", "depends:ghost Work"),
			&tool.TaskRecord{ID: "other", Description: "x", Status: "done"},
		)
		_, err := FromRuntime("p", rt)
		if err == nil || !strings.Contains(err.Error(), "unknown task") {
			t.Fatalf("err = %v, want an unknown-task error", err)
		}
	})

	t.Run("self dependency", func(t *testing.T) {
		rt := newRuntime(pendingTask("a", "depends:a Work"))
		_, err := FromRuntime("p", rt)
		if err == nil || !strings.Contains(err.Error(), "circular") {
			t.Fatalf("err = %v, want a circular-dependency error", err)
		}
	})

	t.Run("two-node cycle", func(t *testing.T) {
		rt := newRuntime(
			pendingTask("a", "depends:b Work A"),
			pendingTask("b", "depends:a Work B"),
		)
		_, err := FromRuntime("p", rt)
		if err == nil || !strings.Contains(err.Error(), "circular") {
			t.Fatalf("err = %v, want a circular-dependency error", err)
		}
	})

	t.Run("three-node cycle", func(t *testing.T) {
		rt := newRuntime(
			pendingTask("a", "depends:c A"),
			pendingTask("b", "depends:a B"),
			pendingTask("c", "depends:b C"),
		)
		_, err := FromRuntime("p", rt)
		if err == nil || !strings.Contains(err.Error(), "circular") {
			t.Fatalf("err = %v, want a circular-dependency error", err)
		}
	})
}

// TestFromRuntimeBareDependsPrefix covers "depends:" with no IDs after it —
// the regex allows an empty capture group, so this must not become a
// dependency on the empty string (which would then fail the unknown-task check
// and make the whole plan unusable).
func TestFromRuntimeBareDependsPrefix(t *testing.T) {
	rt := newRuntime(pendingTask("a", "depends: Just do it"))
	p, err := FromRuntime("p", rt)
	if err != nil {
		t.Fatalf("FromRuntime: %v", err)
	}
	if len(p.Tasks[0].DependsOn) != 0 {
		t.Fatalf("DependsOn = %v, want empty for a bare \"depends:\"", p.Tasks[0].DependsOn)
	}
	if p.Tasks[0].Title != "Just do it" {
		t.Fatalf("Title = %q, want %q", p.Tasks[0].Title, "Just do it")
	}
}

// ---------- topologicalSort ----------

func TestTopologicalSortLevels(t *testing.T) {
	tasks := []*Task{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"a"}},
		{ID: "d", DependsOn: []string{"b", "c"}},
		{ID: "e"}, // independent
	}
	byID := map[string]*Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}

	levels := topologicalSort(tasks, byID)
	if levels == nil {
		t.Fatal("topologicalSort returned nil for an acyclic graph")
	}
	if len(levels) != 3 {
		t.Fatalf("got %d levels, want 3", len(levels))
	}

	levelOf := map[string]int{}
	for i, lvl := range levels {
		for _, task := range lvl {
			levelOf[task.ID] = i
		}
	}
	// a and e have no deps; b/c depend on a; d depends on b and c.
	if levelOf["a"] != 0 || levelOf["e"] != 0 {
		t.Errorf("a=%d e=%d, both want level 0", levelOf["a"], levelOf["e"])
	}
	if levelOf["b"] != 1 || levelOf["c"] != 1 {
		t.Errorf("b=%d c=%d, both want level 1", levelOf["b"], levelOf["c"])
	}
	if levelOf["d"] != 2 {
		t.Errorf("d=%d, want level 2", levelOf["d"])
	}

	// Every task must appear exactly once.
	if len(levelOf) != len(tasks) {
		t.Fatalf("%d tasks placed, want %d", len(levelOf), len(tasks))
	}

	// The invariant that makes the levels usable: a task's deps are all in
	// strictly earlier levels.
	for _, task := range tasks {
		for _, dep := range task.DependsOn {
			if levelOf[dep] >= levelOf[task.ID] {
				t.Errorf("%s (level %d) depends on %s (level %d) — not strictly earlier",
					task.ID, levelOf[task.ID], dep, levelOf[dep])
			}
		}
	}
}

func TestTopologicalSortDetectsCycle(t *testing.T) {
	tasks := []*Task{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	}
	byID := map[string]*Task{"a": tasks[0], "b": tasks[1]}
	if levels := topologicalSort(tasks, byID); levels != nil {
		t.Fatalf("topologicalSort = %v, want nil for a cycle", levels)
	}
}

func TestTopologicalSortEmpty(t *testing.T) {
	if levels := topologicalSort(nil, map[string]*Task{}); len(levels) != 0 {
		t.Fatalf("got %d levels for no tasks, want 0", len(levels))
	}
}

// ---------- FormatResult ----------

func TestFormatResult(t *testing.T) {
	out := FormatResult(&ExecutionResult{
		PlanID: "p1",
		Tasks: []*Task{
			{ID: "a", Title: "Build", Status: "done"},
			{ID: "b", Title: "Test", Status: "failed", Error: "compile error"},
			{ID: "c", Title: "Ship", Status: "skipped", Error: `dependency "b" did not succeed`},
		},
		Success: false,
	})

	for _, want := range []string{
		"p1", "Build", "Test", "Ship",
		"compile error", "did not succeed",
		"✓", "✗", "○",
		"Total: 3 tasks", "Success: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatResult output missing %q:\n%s", want, out)
		}
	}
}
