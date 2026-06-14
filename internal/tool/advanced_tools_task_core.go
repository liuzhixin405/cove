package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type TaskCreateTool struct{ baseTool }
type TaskListTool struct{ baseTool }
type TaskUpdateTool struct{ baseTool }

func NewTaskCreateTool() Tool {
	return &TaskCreateTool{baseTool{def: Def{
		Name: "task", Aliases: []string{"TaskCreate"},
		Description: "Create a background task that runs independently.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, UserFacingName: "Task Create",
	}}}
}
func (t *TaskCreateTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	title, _ := input["title"].(string)
	desc, _ := input["description"].(string)
	id := ""
	if tctx.Runtime != nil {
		tctx.Runtime.Lock()
		ensureRuntimeMaps(tctx.Runtime)
		tctx.Runtime.TaskCounter++
		id = fmt.Sprintf("task-%d", tctx.Runtime.TaskCounter)
		now := time.Now().Format(time.RFC3339)
		tctx.Runtime.Tasks[id] = &TaskRecord{
			ID: id, Title: title, Description: desc, Status: "pending", Kind: "task", CreatedAt: now, UpdatedAt: now,
		}
		tctx.Runtime.Unlock()
	}
	return Result{Data: fmt.Sprintf("Task created [%s]: %s\n%s\nStatus: pending", id, title, desc)}, nil
}
func (t *TaskCreateTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("task creation is safe")
}

func NewTaskListTool() Tool {
	return &TaskListTool{baseTool{def: Def{
		Name: "task_list", Aliases: []string{"TaskList"},
		Description: "List all background tasks and their status.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		IsReadOnly:  true, IsConcurrencySafe: true, UserFacingName: "Task List",
	}}}
}
func (t *TaskListTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if tctx.Runtime == nil || len(tctx.Runtime.Tasks) == 0 {
		return Result{Data: "No active tasks."}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d tasks:\n", len(tctx.Runtime.Tasks)))
	for _, tr := range tctx.Runtime.Tasks {
		mark := "[ ]"
		switch tr.Status {
		case "completed":
			mark = "[✓]"
		case "running":
			mark = "[>]"
		case "cancelled":
			mark = "[x]"
		}
		sb.WriteString(fmt.Sprintf("%s %s: %s (%s)\n", mark, tr.ID, tr.Title, tr.Status))
	}
	return Result{Data: sb.String()}, nil
}
func (t *TaskListTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("task list is read-only")
}

func NewTaskUpdateTool() Tool {
	return &TaskUpdateTool{baseTool{def: Def{
		Name: "task_update", Aliases: []string{"TaskUpdate"},
		Description: "Update a task's status or output.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"},"status":{"type":"string"},"output":{"type":"string"}},"required":["taskId","status"]}`),
		IsReadOnly:  false, UserFacingName: "Task Update",
	}}}
}
func (t *TaskUpdateTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	id, _ := input["taskId"].(string)
	status, _ := input["status"].(string)
	output, _ := input["output"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.Lock()
		if tr, ok := tctx.Runtime.Tasks[id]; ok {
			tr.Status = status
			if output != "" {
				tr.Output = output
			}
			tr.UpdatedAt = time.Now().Format(time.RFC3339)
			tctx.Runtime.Unlock()
			return Result{Data: fmt.Sprintf("Task %s -> %s", id, status)}, nil
		}
		tctx.Runtime.Unlock()
	}
	return Result{Data: fmt.Sprintf("Task %s not found", id), IsError: true}, nil
}
func (t *TaskUpdateTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("task update is safe")
}
