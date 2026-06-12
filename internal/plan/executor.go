package plan

import (
	"context"
	"fmt"
	"sync"

	"github.com/liuzhixin405/cove/internal/delegate"
	"github.com/liuzhixin405/cove/internal/tool"
)

// MaxParallelAgents is the maximum number of sub-agents that can run concurrently.
const MaxParallelAgents = 4

// DefaultMaxRetries is how many times the supervisor re-dispatches a failed
// task (feeding the failure reason back into the next attempt).
const DefaultMaxRetries = 1

// ExecutionResult holds the outcome of a plan execution.
type ExecutionResult struct {
	PlanID  string
	Tasks   []*Task
	Success bool
}

// PlanExecutor executes a Plan using delegate.SubAgent for each task.
// Tasks are scheduled respecting dependencies — independent tasks
// (those at the same BFS depth level) may run concurrently.
type PlanExecutor struct {
	delegator  *delegate.Delegator
	runtime    *tool.Runtime
	maxRetries int
}

// NewPlanExecutor creates a PlanExecutor backed by the given Delegator.
func NewPlanExecutor(d *delegate.Delegator, rt *tool.Runtime) *PlanExecutor {
	return &PlanExecutor{
		delegator:  d,
		runtime:    rt,
		maxRetries: DefaultMaxRetries,
	}
}

// SetMaxRetries configures how many times the supervisor retries a failed task.
func (pe *PlanExecutor) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	pe.maxRetries = n
}

// Execute runs all tasks in the plan.
func (pe *PlanExecutor) Execute(ctx context.Context, plan *Plan) *ExecutionResult {
	taskByID := make(map[string]*Task)
	for _, t := range plan.Tasks {
		taskByID[t.ID] = t
	}

	levels := topologicalSort(plan.Tasks, taskByID)
	if levels == nil {
		// Cycle detected in dependencies
		for _, t := range plan.Tasks {
			t.Status = "failed"
			t.Error = "circular dependency detected"
		}
		return &ExecutionResult{
			PlanID:  plan.ID,
			Tasks:   plan.Tasks,
			Success: false,
		}
	}

	completed := make(map[string]bool)
	allSuccess := true

	for _, level := range levels {
		if plan.Parallel && len(level) > 1 {
			// Execute level concurrently
			var wg sync.WaitGroup
			sem := make(chan struct{}, MaxParallelAgents)
			results := make([]struct {
				task    *Task
				success bool
			}, len(level))

			for i, t := range level {
				wg.Add(1)
				sem <- struct{}{}
				go func(idx int, task *Task) {
					defer wg.Done()
					defer func() { <-sem }()
					success := pe.runTask(ctx, task, completed)
					results[idx] = struct {
						task    *Task
						success bool
					}{task, success}
				}(i, t)
			}
			wg.Wait()

			for _, r := range results {
				completed[r.task.ID] = true
				if !r.success {
					allSuccess = false
				}
			}
		} else {
			// Serial execution
			for _, t := range level {
				success := pe.runTask(ctx, t, completed)
				completed[t.ID] = true
				if !success {
					allSuccess = false
				}
			}
		}

		// If any task failed, mark downstream tasks as skipped
		if !allSuccess {
			for dep := range completed {
				for _, t := range plan.Tasks {
					if t.Status == "pending" {
						for _, d := range t.DependsOn {
							if d == dep && completed[dep] &&
								taskByID[dep].Status == "failed" {
								t.Status = "skipped"
								t.Error = fmt.Sprintf("dependency %q failed", dep)
								break
							}
						}
					}
				}
			}
		}
	}

	return &ExecutionResult{
		PlanID:  plan.ID,
		Tasks:   plan.Tasks,
		Success: allSuccess,
	}
}

// runTask executes a single task via delegate.SubAgent, with supervisor retry.
func (pe *PlanExecutor) runTask(ctx context.Context, task *Task, completed map[string]bool) (success bool) {
	task.Status = "running"

	// Update the runtime task state
	pe.runtime.Lock()
	parentID := ""
	if tr, ok := pe.runtime.Tasks[task.ID]; ok {
		tr.Status = "running"
		parentID = tr.ParentID
	}
	pe.runtime.Unlock()

	// Get context from completed dependencies
	var depOutputs []string
	for _, depID := range task.DependsOn {
		pe.runtime.Lock()
		if tr, ok := pe.runtime.Tasks[depID]; ok && tr.Output != "" {
			depOutputs = append(depOutputs, fmt.Sprintf("[%s output]: %s", depID, tr.Output))
		}
		pe.runtime.Unlock()
	}

	// Actively deliver any messages addressed to this task, its team (parent),
	// or broadcast ("all"). Delivered messages are marked so they are injected
	// once and surface in the agent's prompt instead of being passively logged.
	messages := pe.takeMessagesFor(task.ID, parentID)

	basePrompt := task.Description
	for _, dep := range depOutputs {
		basePrompt += "\n" + dep
	}
	for _, m := range messages {
		basePrompt += "\n[收到消息] " + m
	}

	systemPrompt := "You are a task execution agent. Complete the assigned task efficiently. " +
		"Use available tools to read, write, and modify files. " +
		"Report your results concisely. Do not ask for confirmation — just do the task."

	prompt := basePrompt
	var lastErr string
	for attempt := 0; attempt <= pe.maxRetries; attempt++ {
		if attempt > 0 {
			// Supervisor re-dispatch: feed the prior failure back in.
			prompt = basePrompt + fmt.Sprintf(
				"\n\n[上一次尝试失败 (%d/%d)] 原因: %s\n请修正问题后重试。",
				attempt, pe.maxRetries, lastErr)
		}

		result := pe.delegator.Delegate(ctx, task.ID, prompt, systemPrompt)

		if result == nil {
			lastErr = "delegator returned nil result"
		} else if result.Error != "" {
			lastErr = result.Error
		} else if !result.Success {
			lastErr = "task did not complete successfully"
		} else {
			task.Status = "done"
			task.Output = result.Output
			task.Error = ""
			pe.syncRuntimeTask(task)
			return true
		}

		// Don't retry if the context was cancelled.
		if ctx.Err() != nil {
			break
		}
	}

	task.Status = "failed"
	task.Error = lastErr
	pe.syncRuntimeTask(task)
	return false
}

// takeMessagesFor returns and marks-as-delivered the pending messages addressed
// to the given task ID, its team (parentID), or broadcast targets.
func (pe *PlanExecutor) takeMessagesFor(taskID, parentID string) []string {
	pe.runtime.Lock()
	defer pe.runtime.Unlock()
	var out []string
	for i := range pe.runtime.Messages {
		m := &pe.runtime.Messages[i]
		if m.Delivered {
			continue
		}
		if m.To == taskID || m.To == "all" || (parentID != "" && m.To == parentID) {
			out = append(out, m.Message)
			m.Delivered = true
		}
	}
	return out
}

// syncRuntimeTask copies the task status/output back into the shared runtime.
func (pe *PlanExecutor) syncRuntimeTask(task *Task) {
	pe.runtime.Lock()
	if tr, ok := pe.runtime.Tasks[task.ID]; ok {
		tr.Status = task.Status
		tr.Output = task.Output
	}
	pe.runtime.Unlock()
}
