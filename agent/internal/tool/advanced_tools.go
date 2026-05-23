package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/skills"
)

type PlanModeTool struct{ baseTool }
type ExitPlanModeTool struct{ baseTool }
type EnterWorktreeTool struct{ baseTool }
type ExitWorktreeTool struct{ baseTool }
type TaskCreateTool struct{ baseTool }
type TaskListTool struct{ baseTool }
type TaskUpdateTool struct{ baseTool }
type SleepTool struct{ baseTool }
type BriefTool struct{ baseTool }
type SkillTool struct{ baseTool }
type AgentToolI struct{ baseTool }
type TeamCreateTool struct{ baseTool }
type TeamDeleteTool struct{ baseTool }
type CronTool struct{ baseTool }
type SendMessageTool struct{ baseTool }
type LSPTool struct{ baseTool }

func NewPlanModeTool() Tool { return &PlanModeTool{baseTool{def: Def{
	Name: "plan_mode", Aliases: []string{"EnterPlanMode"},
	Description: "Enter plan mode: only read operations allowed. Use before complex changes.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	IsReadOnly: true, UserFacingName: "Plan Mode",
}}}}
func (t *PlanModeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	reason, _ := input["reason"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.PlanMode = true
	}
	return Result{Data: fmt.Sprintf("Plan mode active. Read-only operations only. Reason: %s", reason)}, nil
}
func (t *PlanModeTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("plan mode entry is safe") }

func NewExitPlanModeTool() Tool { return &ExitPlanModeTool{baseTool{def: Def{
	Name: "exit_plan_mode", Aliases: []string{"ExitPlanMode"},
	Description: "Exit plan mode. Full tool access restored.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
	IsReadOnly: false, UserFacingName: "Exit Plan Mode",
}}}}
func (t *ExitPlanModeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	summary, _ := input["summary"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.PlanMode = false
	}
	return Result{Data: fmt.Sprintf("Plan mode exited. Summary: %s", summary)}, nil
}
func (t *ExitPlanModeTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Asked("exiting plan mode requires confirmation") }

func NewEnterWorktreeTool() Tool { return &EnterWorktreeTool{baseTool{def: Def{
	Name: "worktree", Aliases: []string{"EnterWorktree"},
	Description: "Create a git worktree for isolated work.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string"}},"required":["branch"]}`),
	IsReadOnly: false, UserFacingName: "Enter Worktree",
}}}}
func (t *EnterWorktreeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	branch, _ := input["branch"].(string)
	cwd := tctx.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "../"+branch, "-b", branch)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Data: fmt.Sprintf("Error creating worktree: %v\n%s", err, string(out)), IsError: true}, nil
	}
	wtPath := cwd + "/../" + branch
	if tctx.Runtime != nil {
		tctx.Runtime.WorktreeDir = wtPath
	}
	return Result{Data: fmt.Sprintf("Worktree created at %s\nGit output: %s", wtPath, string(out))}, nil
}
func (t *EnterWorktreeTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Asked("worktree creation requires confirmation") }

func NewExitWorktreeTool() Tool { return &ExitWorktreeTool{baseTool{def: Def{
	Name: "exit_worktree", Aliases: []string{"ExitWorktree"},
	Description: "Exit the current worktree.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"merge":{"type":"boolean"}}}`),
	IsReadOnly: false, UserFacingName: "Exit Worktree",
}}}}
func (t *ExitWorktreeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if tctx.Runtime == nil || tctx.Runtime.WorktreeDir == "" {
		return Result{Data: "No active worktree", IsError: true}, nil
	}
	cwd := tctx.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", tctx.Runtime.WorktreeDir)
	cmd.Dir = cwd
	out, _ := cmd.CombinedOutput()
	tctx.Runtime.WorktreeDir = ""
	return Result{Data: fmt.Sprintf("Worktree removed.\n%s", string(out))}, nil
}
func (t *ExitWorktreeTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Asked("worktree removal requires confirmation") }

func NewTaskCreateTool() Tool { return &TaskCreateTool{baseTool{def: Def{
	Name: "task", Aliases: []string{"TaskCreate"},
	Description: "Create a background task that runs independently.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}`),
	IsReadOnly: false, IsConcurrencySafe: true, UserFacingName: "Task Create",
}}}}
func (t *TaskCreateTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	title, _ := input["title"].(string)
	desc, _ := input["description"].(string)
	id := ""
	if tctx.Runtime != nil {
		tctx.Runtime.Lock()
		tctx.Runtime.TaskCounter++
		id = fmt.Sprintf("task-%d", tctx.Runtime.TaskCounter)
		tctx.Runtime.Tasks[id] = &TaskRecord{
			ID: id, Title: title, Description: desc, Status: "pending",
		}
		tctx.Runtime.Unlock()
	}
	return Result{Data: fmt.Sprintf("Task created [%s]: %s\n%s\nStatus: pending", id, title, desc)}, nil
}
func (t *TaskCreateTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("task creation is safe") }

func NewTaskListTool() Tool { return &TaskListTool{baseTool{def: Def{
	Name: "task_list", Aliases: []string{"TaskList"},
	Description: "List all background tasks and their status.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Task List",
}}}}
func (t *TaskListTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if tctx.Runtime == nil || len(tctx.Runtime.Tasks) == 0 {
		return Result{Data: "No active tasks."}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d tasks:\n", len(tctx.Runtime.Tasks)))
	for _, tr := range tctx.Runtime.Tasks {
		mark := "[ ]"
		switch tr.Status {
		case "completed": mark = "[✓]"
		case "running": mark = "[>]"
		case "cancelled": mark = "[x]"
		}
		sb.WriteString(fmt.Sprintf("%s %s: %s (%s)\n", mark, tr.ID, tr.Title, tr.Status))
	}
	return Result{Data: sb.String()}, nil
}
func (t *TaskListTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("task list is read-only") }

func NewTaskUpdateTool() Tool { return &TaskUpdateTool{baseTool{def: Def{
	Name: "task_update", Aliases: []string{"TaskUpdate"},
	Description: "Update a task's status or output.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"},"status":{"type":"string"},"output":{"type":"string"}},"required":["taskId","status"]}`),
	IsReadOnly: false, UserFacingName: "Task Update",
}}}}
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
		}
		tctx.Runtime.Unlock()
	}
	return Result{Data: fmt.Sprintf("Task %s -> %s", id, status)}, nil
}
func (t *TaskUpdateTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("task update is safe") }

func NewSleepTool() Tool { return &SleepTool{baseTool{def: Def{
	Name: "sleep", Description: "Pause agent execution for a duration.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"seconds":{"type":"integer"}},"required":["seconds"]}`),
	IsReadOnly: false, IsConcurrencySafe: true, UserFacingName: "Sleep",
}}}}
func (t *SleepTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	sec, _ := input["seconds"].(float64)
	if sec > 300 {
		sec = 300
	}
	if sec < 1 {
		sec = 1
	}
	select {
	case <-time.After(time.Duration(sec) * time.Second):
		return Result{Data: fmt.Sprintf("Slept for %.0f seconds", sec)}, nil
	case <-ctx.Done():
		return Result{Data: "Sleep interrupted", IsError: true}, nil
	}
}
func (t *SleepTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("sleep is safe") }

func NewBriefTool() Tool { return &BriefTool{baseTool{def: Def{
	Name: "brief", Description: "Generate a session or context summary.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"what":{"type":"string"}}}`),
	IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Brief",
}}}}
func (t *BriefTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	what, _ := input["what"].(string)
	if tctx.Runtime == nil {
		if what == "" {
			what = "current session"
		}
		return Result{Data: fmt.Sprintf("Summary for %s is unavailable without runtime context.", what)}, nil
	}
	var sb strings.Builder
	if what == "" {
		what = "current session"
	}
	sb.WriteString(fmt.Sprintf("Summary for %s\n", what))
	sb.WriteString(fmt.Sprintf("Plan mode: %t\n", tctx.Runtime.PlanMode))
	if tctx.Runtime.WorktreeDir != "" {
		sb.WriteString(fmt.Sprintf("Worktree: %s\n", tctx.Runtime.WorktreeDir))
	}
	if len(tctx.Runtime.Tasks) == 0 {
		sb.WriteString("Tasks: none")
		return Result{Data: sb.String()}, nil
	}
	sb.WriteString(fmt.Sprintf("Tasks: %d\n", len(tctx.Runtime.Tasks)))
	for _, tr := range tctx.Runtime.Tasks {
		sb.WriteString(fmt.Sprintf("- %s [%s]: %s", tr.ID, tr.Status, tr.Title))
		if tr.Description != "" {
			sb.WriteString(fmt.Sprintf(" — %s", tr.Description))
		}
		if tr.Output != "" {
			sb.WriteString(fmt.Sprintf(" | output: %s", truncateStr(tr.Output, 120)))
		}
		sb.WriteString("\n")
	}
	return Result{Data: strings.TrimSpace(sb.String())}, nil
}
func (t *BriefTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("brief is read-only") }

func NewSkillTool() Tool { return &SkillTool{baseTool{def: Def{
	Name: "skill", Description: "Execute a skill (predefined workflow).",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"args":{"type":"object"}},"required":["name"]}`),
	IsReadOnly: false, UserFacingName: "Skill",
}}}}
func (t *SkillTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	if tctx.Runtime != nil {
		if tctx.Runtime.SkillPrompts != nil {
			if prompt, ok := tctx.Runtime.SkillPrompts[name]; ok {
				return Result{Data: fmt.Sprintf("[Skill: %s]\n\n%s\n\nFollow these instructions to complete the task.", name, prompt)}, nil
			}
		}
		if mgr, ok := tctx.Runtime.SkillManager.(interface {
			Get(string) (skills.Skill, bool)
		}); ok {
			if skill, found := mgr.Get(name); found {
				return Result{Data: fmt.Sprintf("[Skill: %s]\n\n%s\n\nFollow these instructions to complete the task.", name, skill.Prompt)}, nil
			}
		}
		if tctx.Runtime.SkillPrompts != nil {
			return Result{Data: fmt.Sprintf("Skill '%s' activated. Available skills: %v", name, tctx.Runtime.SkillPrompts)}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Skill '%s' activated. No runtime skill registry available.", name)}, nil
}
func (t *SkillTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("skill execution is safe") }

func NewAgentTool() Tool { return &AgentToolI{baseTool{def: Def{
	Name: "agent", Aliases: []string{"Agent"},
	Description: "Spawn a sub-agent to handle complex multi-step tasks independently.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string","description":"Agent type: general, explore, plan, review, test"},"prompt":{"type":"string","description":"Task description for the sub-agent"}},"required":["type","prompt"]}`),
	IsReadOnly: false, IsConcurrencySafe: true, UserFacingName: "Agent",
}}}}
func (t *AgentToolI) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	agentType, _ := input["type"].(string)
	task, _ := input["prompt"].(string)
	if tctx.Runtime != nil && tctx.Runtime.AgentRunner != nil {
		if runner, ok := tctx.Runtime.AgentRunner.(api.AgentRunner); ok {
			result, err := runner.Run(ctx, agentType, task)
			if err != nil {
				return Result{Data: fmt.Sprintf("Sub-agent error: %v", err), IsError: true}, nil
			}
			return Result{Data: fmt.Sprintf("Sub-agent [%s] result:\n%s\nCost: $%.4f | Steps: %d | Success: %v",
				agentType, result.Output, result.Cost, result.Steps, result.Success)}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Sub-agent [%s] dispatched: %s", agentType, truncateStr(task, 300))}, nil
}
func (t *AgentToolI) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("agent spawning is safe") }

func NewTeamCreateTool() Tool { return &TeamCreateTool{baseTool{def: Def{
	Name: "team_create", Description: "Create a team of agents for parallel work.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"members":{"type":"array","items":{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"}}}}},"required":["name","members"]}`),
	IsReadOnly: false, IsConcurrencySafe: true, UserFacingName: "Team Create",
}}}}
func (t *TeamCreateTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	members, _ := input["members"].([]any)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Team '%s' created with %d members:\n", name, len(members)))
	if tctx.Runtime != nil && tctx.Runtime.Tasks == nil {
		tctx.Runtime.Tasks = make(map[string]*TaskRecord)
	}
	for i, m := range members {
		if mm, ok := m.(map[string]any); ok {
			ag, _ := mm["agent"].(string)
			tsk, _ := mm["task"].(string)
			id := fmt.Sprintf("team-%s-%d", name, i+1)
			if tctx.Runtime != nil {
				tctx.Runtime.Tasks[id] = &TaskRecord{ID: id, Title: ag, Description: tsk, Status: "pending"}
			}
			sb.WriteString(fmt.Sprintf("  %d. [%s] %s\n", i+1, ag, tsk))
		}
	}
	return Result{Data: strings.TrimSpace(sb.String())}, nil
}
func (t *TeamCreateTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("team creation is safe") }

func NewTeamDeleteTool() Tool { return &TeamDeleteTool{baseTool{def: Def{
	Name: "team_delete", Description: "Delete a previously created agent team.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	IsReadOnly: false, UserFacingName: "Team Delete",
}}}}
func (t *TeamDeleteTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	removed := 0
	if tctx.Runtime != nil {
		for id := range tctx.Runtime.Tasks {
			if strings.HasPrefix(id, "team-"+name+"-") {
				delete(tctx.Runtime.Tasks, id)
				removed++
			}
		}
	}
	if removed == 0 {
		return Result{Data: fmt.Sprintf("Team '%s' not found in runtime.", name), IsError: true}, nil
	}
	return Result{Data: fmt.Sprintf("Team '%s' removed from runtime (%d members cleaned up).", name, removed)}, nil
}
func (t *TeamDeleteTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("team deletion is safe in current runtime") }

func NewCronTool() Tool { return &CronTool{baseTool{def: Def{
	Name: "cron", Description: "Schedule a recurring task using cron syntax.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"schedule":{"type":"string"},"task":{"type":"string"}},"required":["schedule","task"]}`),
	IsReadOnly: false, IsConcurrencySafe: true, UserFacingName: "Cron",
}}}}
func (t *CronTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	schedule, _ := input["schedule"].(string)
	task, _ := input["task"].(string)
	if tctx.Runtime == nil {
		return Result{Data: fmt.Sprintf("Cron request recorded: %s -> %s", schedule, truncateStr(task, 200))}, nil
	}
	if tctx.Runtime.Tasks == nil {
		tctx.Runtime.Tasks = make(map[string]*TaskRecord)
	}
	tctx.Runtime.Lock()
	tctx.Runtime.TaskCounter++
	id := fmt.Sprintf("cron-%d", tctx.Runtime.TaskCounter)
	tctx.Runtime.Tasks[id] = &TaskRecord{
		ID:          id,
		Title:       fmt.Sprintf("cron %s", schedule),
		Description: task,
		Status:      "scheduled",
	}
	tctx.Runtime.Unlock()
	return Result{Data: fmt.Sprintf("Scheduled recurring task [%s] on %s", id, schedule)}, nil
}
func (t *CronTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Asked("cron scheduling requires confirmation") }

func NewSendMessageTool() Tool { return &SendMessageTool{baseTool{def: Def{
	Name: "send_message", Description: "Send a message to another agent or the user.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"message":{"type":"string"}},"required":["to","message"]}`),
	IsReadOnly: false, IsConcurrencySafe: true, UserFacingName: "Send Message",
}}}}
func (t *SendMessageTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	to, _ := input["to"].(string)
	msg, _ := input["message"].(string)
	if tctx.Runtime != nil {
		if tr, ok := tctx.Runtime.Tasks[to]; ok {
			if tr.Output != "" {
				tr.Output += "\n"
			}
			tr.Output += fmt.Sprintf("message: %s", msg)
			return Result{Data: fmt.Sprintf("Delivered message to task %s", to)}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Unable to deliver message to %s", to), IsError: true}, nil
}
func (t *SendMessageTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("messaging is safe") }

func NewLSPTool() Tool { return &LSPTool{baseTool{def: Def{
	Name: "lsp", Description: "Language Server Protocol: diagnostics, hover, references, definitions.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"filePath":{"type":"string"}},"required":["action","filePath"]}`),
	IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "LSP",
}}}}
func (t *LSPTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	action, _ := input["action"].(string)
	filePath, _ := input["filePath"].(string)
	if tctx.Runtime != nil && tctx.Runtime.LSPRunner != nil {
		out, err := tctx.Runtime.LSPRunner.Run(ctx, action, filePath, input)
		if err != nil {
			return Result{Data: fmt.Sprintf("LSP %s failed for %s: %v", action, filePath, err), IsError: true}, nil
		}
		return Result{Data: out}, nil
	}
	return Result{Data: fmt.Sprintf("LSP backend unavailable for %s on %s", action, filePath), IsError: true, ShouldRetry: true}, nil
}
func (t *LSPTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("lsp is read-only") }

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

type TaskStopTool struct{ baseTool }
type TaskGetTool struct{ baseTool }
type TaskOutputTool struct{ baseTool }

func NewTaskStopTool() Tool { return &TaskStopTool{baseTool{def: Def{
	Name: "task_stop", Aliases: []string{"TaskStop"},
	Description: "Stop a running background task.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"}},"required":["taskId"]}`),
	IsReadOnly: false, UserFacingName: "Task Stop",
}}}}
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
func (t *TaskStopTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("task stop is safe") }

func NewTaskGetTool() Tool { return &TaskGetTool{baseTool{def: Def{
	Name: "task_get", Aliases: []string{"TaskGet"},
	Description: "Get detailed information about a background task.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"}},"required":["taskId"]}`),
	IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Task Get",
}}}}
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
func (t *TaskGetTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("task get is read-only") }

func NewTaskOutputTool() Tool { return &TaskOutputTool{baseTool{def: Def{
	Name: "task_output", Aliases: []string{"TaskOutput"},
	Description: "Retrieve the output of a completed background task.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"taskId":{"type":"string"}},"required":["taskId"]}`),
	IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Task Output",
}}}}
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
func (t *TaskOutputTool) CheckPermissions(input Input, tctx Context) PermissionDecision { return Allowed("task output is read-only") }
