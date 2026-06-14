package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

type TaskStopTool struct{ baseTool }
type TaskGetTool struct{ baseTool }
type TaskOutputTool struct{ baseTool }

func NewTaskStopTool() Tool {
	return &TaskStopTool{baseTool{def: Def{
		Name: "task_stop", Aliases: []string{"TaskStop"},
		Description: "Stop a running background task.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"}},"required":["taskId"]}`),
		IsReadOnly:  false, UserFacingName: "Task Stop",
	}}}
}
func (t *TaskStopTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	id, _ := input["taskId"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.Lock()
		tr, ok := tctx.Runtime.Tasks[id]
		if ok {
			tr.Status = "cancelled"
		}
		tctx.Runtime.Unlock()
		if ok {
			return Result{Data: fmt.Sprintf("Task %s stopped: %s", id, tr.Title)}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Task %s stopped.", id)}, nil
}
func (t *TaskStopTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("task stop is safe")
}

func NewTaskGetTool() Tool {
	return &TaskGetTool{baseTool{def: Def{
		Name: "task_get", Aliases: []string{"TaskGet"},
		Description: "Get detailed information about a background task.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"}},"required":["taskId"]}`),
		IsReadOnly:  true, IsConcurrencySafe: true, UserFacingName: "Task Get",
	}}}
}
func (t *TaskGetTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	id, _ := input["taskId"].(string)
	if tctx.Runtime != nil {
		if tr, ok := tctx.Runtime.Tasks[id]; ok {
			return Result{Data: fmt.Sprintf("Task %s:\n  Title: %s\n  Status: %s\n  Description: %s\n  Output: %s",
				id, tr.Title, tr.Status, tr.Description, tr.Output)}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Task %s not found", id), IsError: true}, nil
}
func (t *TaskGetTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("task get is read-only")
}

func NewTaskOutputTool() Tool {
	return &TaskOutputTool{baseTool{def: Def{
		Name: "task_output", Aliases: []string{"TaskOutput"},
		Description: "Retrieve the output of a completed background task.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"}},"required":["taskId"]}`),
		IsReadOnly:  true, IsConcurrencySafe: true, UserFacingName: "Task Output",
	}}}
}
func (t *TaskOutputTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	id, _ := input["taskId"].(string)
	if tctx.Runtime != nil {
		if tr, ok := tctx.Runtime.Tasks[id]; ok {
			if tr.Output == "" {
				return Result{Data: fmt.Sprintf("Task %s has no output yet (status: %s)", id, tr.Status)}, nil
			}
			return Result{Data: tr.Output}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Task %s not found", id), IsError: true}, nil
}
func (t *TaskOutputTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("task output is read-only")
}

// --- execute_plan ---

type ExecutePlanTool struct{ baseTool }

func NewExecutePlanTool() Tool {
	return &ExecutePlanTool{baseTool{def: Def{
		Name: "execute_plan", Aliases: []string{"ExecutePlan"},
		Description: "Execute all pending tasks defined by todowrite, respecting dependencies. " +
			"Use ONLY for complex multi-step implementation plans (3+ tasks). " +
			"Do NOT use for simple Q&A, single-file edits, or casual conversation. " +
			"Independent tasks (same dependency level) run concurrently when parallel=true.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"parallel":{"type":"boolean","description":"Run independent tasks concurrently when true"},
				"max_agents":{"type":"integer","description":"Maximum concurrent agents (default 4)"}
			},
			"required":[]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Execute Plan",
	}}}
}

func (t *ExecutePlanTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	parallel, _ := input["parallel"].(bool)

	if tctx.Runtime != nil && tctx.Runtime.PlanExecuteFunc != nil {
		result, err := tctx.Runtime.PlanExecuteFunc(ctx, parallel)
		if err != nil {
			return Result{Data: err.Error(), IsError: true}, nil
		}
		return Result{Data: result}, nil
	}
	return Result{Data: "Plan executor not available. Execute the tasks listed in " +
		"todowrite one at a time using the standard tools (read, write, bash, etc.). " +
		"Work through them in dependency order."}, nil
}

func (t *ExecutePlanTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("plan execution has no safety risk")
}
