package plan

import (
	"strings"
	"testing"
)

// The dependency prefix allowed no spaces: "depends:task-1, task-2 Create auth
// tests" gave deps [task-1] and the title ", task-2 Create auth tests", and
// "depends: task-1 do x" had no dependencies at all, so the task ran before
// the one it needs.
func TestFromRuntimeAllowsSpacesInDependsList(t *testing.T) {
	cases := []struct {
		desc      string
		wantDeps  []string
		wantTitle string
	}{
		{"depends:task-1, task-2 Create auth tests", []string{"task-1", "task-2"}, "Create auth tests"},
		{"depends: task-1 do x", []string{"task-1"}, "do x"},
		{"depends : task-1 ,task-2 do y", []string{"task-1", "task-2"}, "do y"},
		{"depends:task-1,task-2 do z", []string{"task-1", "task-2"}, "do z"},
	}
	for _, c := range cases {
		rt := newRuntime(pendingTask("task-1", "one"), pendingTask("task-2", "two"), pendingTask("x", c.desc))
		p, err := FromRuntime("p", rt)
		if err != nil {
			t.Fatalf("%q: %v", c.desc, err)
		}
		var got *Task
		for _, task := range p.Tasks {
			if task.ID == "x" {
				got = task
			}
		}
		if strings.Join(got.DependsOn, ",") != strings.Join(c.wantDeps, ",") || got.Title != c.wantTitle {
			t.Errorf("%q: deps=%v title=%q, want %v %q", c.desc, got.DependsOn, got.Title, c.wantDeps, c.wantTitle)
		}
	}
}
