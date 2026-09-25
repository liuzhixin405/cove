// Package plan provides a Plan Executor that reads tasks from the
// todowrite tool's Runtime.Tasks state, builds a dependency DAG,
// and executes them via sub-agents (delegate.Delegator).
package plan

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/delegate"
	"github.com/liuzhixin405/cove/internal/tool"
)

// DepPrefix is the convention used in todowrite "content" fields
// to declare dependencies on other task IDs.
// Example: "depends:task-1,task-2 Create auth tests"
const DepPrefix = "depends:"

var depRe = regexp.MustCompile(`^depends:([\w\-]+(?:,[\w\-]+)*)?\s*`)

// Task represents one executable node in a plan.
type Task struct {
	ID          string   // matches Runtime.Tasks key
	Title       string   // display title (content without depends: prefix)
	Description string   // original todowrite content
	Status      string   // pending | running | done | failed | skipped
	DependsOn   []string // task IDs this task depends on
	AgentType   string   // general / plan / explore / review / test
	MaxIter     int      // max iterations for sub-agent, 0 = default (20)
	Output      string
	Error       string
	// ExitReason is the last sub-agent run's delegate.Exit* reason; empty
	// when no sub-agent ran (skipped, cancelled before start).
	ExitReason string
}

// Plan holds a set of tasks with dependency ordering.
type Plan struct {
	ID       string
	Goal     string
	Tasks    []*Task
	Parallel bool // allow independent tasks (same depth) to run concurrently
}

// FromRuntime builds a Plan by scanning Runtime.Tasks for pending tasks
// and parsing dependency declarations from the description field.
func FromRuntime(planID string, rt *tool.Runtime) (*Plan, error) {
	rt.Lock()
	defer rt.Unlock()

	if len(rt.Tasks) == 0 {
		return nil, fmt.Errorf("no tasks found. Use todowrite to define tasks first")
	}

	var tasks []*Task
	taskByID := make(map[string]*Task)
	seen := make(map[string]bool)

	for id, tr := range rt.Tasks {
		if tr.Status != "pending" {
			continue
		}
		content := strings.TrimSpace(tr.Description)
		if content == "" {
			continue
		}

		title := content
		var deps []string

		if m := depRe.FindStringSubmatch(content); m != nil {
			if m[1] != "" {
				deps = strings.Split(m[1], ",")
				for i := range deps {
					deps[i] = strings.TrimSpace(deps[i])
				}
			}
			title = strings.TrimSpace(content[len(m[0]):])
		}
		if title == "" {
			title = content
		}

		t := &Task{
			ID:          id,
			Title:       title,
			Description: content,
			Status:      "pending",
			DependsOn:   deps,
			AgentType:   "general",
			MaxIter:     20,
		}
		tasks = append(tasks, t)
		taskByID[id] = t
		seen[id] = true
	}

	if len(tasks) == 0 {
		return nil, fmt.Errorf("no pending tasks found")
	}

	// Validate dependencies exist and detect cycles. A dependency outside the
	// plan is fine when that task already finished: a plan resumed after its
	// first steps completed used to be rejected with "unknown task" for them.
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if seen[dep] {
				continue
			}
			tr, ok := rt.Tasks[dep]
			if !ok {
				return nil, fmt.Errorf("task %q depends on unknown task %q", t.ID, dep)
			}
			if !isFinished(tr.Status) {
				return nil, fmt.Errorf("task %q depends on task %q, which is %s, not completed", t.ID, dep, tr.Status)
			}
		}
	}

	if cycle := detectCycle(tasks, taskByID); cycle != nil {
		return nil, fmt.Errorf("circular dependency detected: %s", strings.Join(cycle, " → "))
	}

	return &Plan{
		ID:    planID,
		Tasks: tasks,
	}, nil
}

// isFinished reports whether a runtime task status means the work is done:
// the executor writes "done", todowrite uses "completed".
func isFinished(status string) bool {
	return status == "done" || status == "completed"
}

// TopologicalSort returns tasks grouped by depth level (BFS).
// Level 0: tasks with no dependencies.
// Level N: tasks whose dependencies are all in levels < N.
// Returns nil if a cycle is detected.
func topologicalSort(tasks []*Task, taskByID map[string]*Task) [][]*Task {
	remaining := make(map[string]*Task)
	for _, t := range tasks {
		remaining[t.ID] = t
	}

	var levels [][]*Task

	for len(remaining) > 0 {
		// Collect the whole ready set BEFORE removing any of it.
		//
		// The previous version deleted from `remaining` inside the same range
		// loop that tested against it, so whether a task's dependency still
		// counted as "remaining" depended on Go's randomized map iteration
		// order. The same graph produced a different number of levels on every
		// run, and a task routinely landed in the same level as its own
		// dependency — which, with Plan.Parallel, means they execute
		// CONCURRENTLY. The dependency graph was effectively ignored.
		var ready []*Task
		for _, t := range remaining {
			allDepsSatisfied := true
			for _, dep := range t.DependsOn {
				if _, stillRemaining := remaining[dep]; stillRemaining {
					allDepsSatisfied = false
					break
				}
			}
			if allDepsSatisfied {
				ready = append(ready, t)
			}
		}
		if len(ready) == 0 {
			// Cycle detected
			return nil
		}
		for _, t := range ready {
			delete(remaining, t.ID)
		}
		// Map iteration also randomizes the order WITHIN a level. Sort so a
		// plan's serial execution order is reproducible run to run.
		sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
		levels = append(levels, ready)
	}

	return levels
}

// detectCycle returns the first cycle path found, or nil.
func detectCycle(tasks []*Task, taskByID map[string]*Task) []string {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	var dfs func(id string, path []string) []string
	dfs = func(id string, path []string) []string {
		visited[id] = true
		recStack[id] = true
		path = append(path, id)

		t := taskByID[id]
		for _, dep := range t.DependsOn {
			if _, inPlan := taskByID[dep]; !inPlan {
				continue // an already-finished task outside the plan
			}
			if recStack[dep] {
				// Found cycle
				cycleStart := -1
				for i, p := range path {
					if p == dep {
						cycleStart = i
						break
					}
				}
				return append(path[cycleStart:], dep)
			}
			if !visited[dep] {
				if cycle := dfs(dep, path); cycle != nil {
					return cycle
				}
			}
		}
		recStack[id] = false
		return nil
	}

	for _, t := range tasks {
		if !visited[t.ID] {
			if cycle := dfs(t.ID, nil); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// partialOutputRunes bounds the partial result FormatResult shows per task.
const partialOutputRunes = 1500

// FormatResult formats an execution result as a human-readable summary.
func FormatResult(result *ExecutionResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan '%s' executed:\n", result.PlanID)
	for _, t := range result.Tasks {
		icon := "✓"
		if t.Status == "failed" {
			icon = "✗"
		}
		if t.Status == "skipped" || t.Status == "cancelled" {
			icon = "○"
		}
		fmt.Fprintf(&sb, "  %s [%s] %s — %s", icon, t.ID, t.Title, t.Status)
		if t.ExitReason != "" && t.ExitReason != delegate.ExitCompleted {
			fmt.Fprintf(&sb, " [exit: %s]", t.ExitReason)
		}
		if t.Error != "" {
			fmt.Fprintf(&sb, " (%s)", t.Error)
		}
		sb.WriteString("\n")
		if t.Status == "failed" && strings.TrimSpace(t.Output) != "" {
			// The partial result of a sub-agent stopped at its cap.
			out := []rune(strings.TrimSpace(t.Output))
			if len(out) > partialOutputRunes {
				out = append(out[:partialOutputRunes], []rune("…")...)
			}
			sb.WriteString("    部分结果:\n    " + strings.ReplaceAll(string(out), "\n", "\n    ") + "\n")
		}
	}
	sb.WriteString("\n" + summaryLine(result.Tasks))
	fmt.Fprintf(&sb, "\nTotal: %d tasks | Success: %v", len(result.Tasks), result.Success)
	return sb.String()
}

// outcomeOrder lists summaryLine's buckets in display order.
var outcomeOrder = []struct{ key, label string }{
	{"done", "个任务完成"},
	{"cap", "个到达上限（含部分结果）"},
	{"loop", "个陷入循环"},
	{"interrupted", "个已中断"},
	{"failed", "个失败"},
	{"skipped", "个跳过"},
}

// taskOutcome classifies a finished task for the summary line.
func taskOutcome(t *Task) string {
	switch {
	case t.Status == "done":
		return "done"
	case t.Status == "skipped":
		return "skipped"
	case t.Status == "cancelled" || t.ExitReason == delegate.ExitInterrupted:
		return "interrupted"
	case t.ExitReason == delegate.ExitMaxIterations:
		return "cap"
	case t.ExitReason == delegate.ExitLoop:
		return "loop"
	default:
		return "failed"
	}
}

// summaryLine counts tasks by outcome, e.g.
// "汇总：3 个任务完成，1 个到达上限（含部分结果），1 个失败".
func summaryLine(tasks []*Task) string {
	counts := map[string]int{}
	for _, t := range tasks {
		counts[taskOutcome(t)]++
	}
	var parts []string
	for _, o := range outcomeOrder {
		if n := counts[o.key]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, o.label))
		}
	}
	if len(parts) == 0 {
		return "汇总：无任务"
	}
	return "汇总：" + strings.Join(parts, "，")
}
