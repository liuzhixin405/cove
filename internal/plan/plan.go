// Package plan provides a Plan Executor that reads tasks from the
// todowrite tool's Runtime.Tasks state, builds a dependency DAG,
// and executes them via sub-agents (delegate.Delegator).
package plan

import (
	"fmt"
	"regexp"
	"strings"

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

	if rt.Tasks == nil || len(rt.Tasks) == 0 {
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

	// Validate dependencies exist and detect cycles
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if !seen[dep] {
				return nil, fmt.Errorf("task %q depends on unknown task %q", t.ID, dep)
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
		var ready []*Task
		for id, t := range remaining {
			allDepsSatisfied := true
			for _, dep := range t.DependsOn {
				if _, stillRemaining := remaining[dep]; stillRemaining {
					allDepsSatisfied = false
					break
				}
			}
			if allDepsSatisfied {
				ready = append(ready, t)
				delete(remaining, id)
			}
		}
		if len(ready) == 0 {
			// Cycle detected
			return nil
		}
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

// FormatResult formats an execution result as a human-readable summary.
func FormatResult(result *ExecutionResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Plan '%s' executed:\n", result.PlanID))
	for _, t := range result.Tasks {
		icon := "✓"
		if t.Status == "failed" {
			icon = "✗"
		}
		if t.Status == "skipped" {
			icon = "○"
		}
		sb.WriteString(fmt.Sprintf("  %s [%s] %s — %s", icon, t.ID, t.Title, t.Status))
		if t.Error != "" {
			sb.WriteString(fmt.Sprintf(" (%s)", t.Error))
		}
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\nTotal: %d tasks | Success: %v", len(result.Tasks), result.Success))
	return sb.String()
}
