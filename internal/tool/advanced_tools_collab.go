package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type TeamCreateTool struct{ baseTool }
type TeamDeleteTool struct{ baseTool }
type CronTool struct{ baseTool }
type SendMessageTool struct{ baseTool }
type LSPTool struct{ baseTool }

func NewTeamCreateTool() Tool {
	return &TeamCreateTool{baseTool{def: Def{
		Name: "team_create", Description: "Create a team of agents for parallel work.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"members":{"type":"array","items":{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"}}}}},"required":["name","members"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, UserFacingName: "Team Create",
	}}}
}
func (t *TeamCreateTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	members, _ := input["members"].([]any)
	if strings.TrimSpace(name) == "" {
		return Result{Data: "team name required", IsError: true}, nil
	}
	if len(members) == 0 {
		return Result{Data: "team requires at least one member", IsError: true}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Team '%s' created with %d members:\n", name, len(members)))
	now := time.Now().Format(time.RFC3339)
	memberRecords := make([]TeamMemberRecord, 0, len(members))
	if tctx.Runtime != nil {
		tctx.Runtime.Lock()
		ensureRuntimeMaps(tctx.Runtime)
	}
	for i, m := range members {
		if mm, ok := m.(map[string]any); ok {
			ag, _ := mm["agent"].(string)
			tsk, _ := mm["task"].(string)
			id := fmt.Sprintf("team-%s-%d", name, i+1)
			memberRecords = append(memberRecords, TeamMemberRecord{ID: id, Agent: ag, Task: tsk, Status: "pending"})
			if tctx.Runtime != nil {
				tctx.Runtime.Tasks[id] = &TaskRecord{ID: id, Title: ag, Description: tsk, Status: "pending", Kind: "team_member", ParentID: name, CreatedAt: now, UpdatedAt: now}
			}
			sb.WriteString(fmt.Sprintf("  %d. [%s] %s\n", i+1, ag, tsk))
		}
	}
	if tctx.Runtime != nil {
		tctx.Runtime.Teams[name] = &TeamRecord{Name: name, Members: memberRecords, Status: "active", CreatedAt: now}
		tctx.Runtime.Unlock()
	}
	if tctx.Runtime != nil {
		if len(tctx.Runtime.Teams) > 0 {
			sb.WriteString(fmt.Sprintf("Teams: %d\n", len(tctx.Runtime.Teams)))
			for _, team := range tctx.Runtime.Teams {
				sb.WriteString(fmt.Sprintf("- %s [%s]: %d members\n", team.Name, team.Status, len(team.Members)))
			}
		}
		if len(tctx.Runtime.CronSchedules) > 0 {
			sb.WriteString(fmt.Sprintf("Cron schedules: %d\n", len(tctx.Runtime.CronSchedules)))
			for _, cron := range tctx.Runtime.CronSchedules {
				sb.WriteString(fmt.Sprintf("- %s [%s]: %s -> %s\n", cron.ID, cron.Status, cron.Schedule, cron.Task))
			}
		}
		if len(tctx.Runtime.Messages) > 0 {
			sb.WriteString(fmt.Sprintf("Messages: %d queued\n", len(tctx.Runtime.Messages)))
		}
	}

	if tctx.Runtime != nil && tctx.Runtime.PlanExecuteFunc != nil {
		result, err := tctx.Runtime.PlanExecuteFunc(true)
		if err != nil {
			sb.WriteString(fmt.Sprintf("\n\n[执行失败] %v", err))
		} else {
			sb.WriteString("\n\n[团队执行结果]\n" + result)
		}
	}

	return Result{Data: strings.TrimSpace(sb.String())}, nil
}
func (t *TeamCreateTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("team creation is safe")
}

func NewTeamDeleteTool() Tool {
	return &TeamDeleteTool{baseTool{def: Def{
		Name: "team_delete", Description: "Delete a previously created agent team.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		IsReadOnly:  false, UserFacingName: "Team Delete",
	}}}
}
func (t *TeamDeleteTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	removed := 0
	if tctx.Runtime != nil {
		tctx.Runtime.Lock()
		for id := range tctx.Runtime.Tasks {
			if strings.HasPrefix(id, "team-"+name+"-") {
				delete(tctx.Runtime.Tasks, id)
				removed++
			}
		}
		if tctx.Runtime.Teams != nil {
			if _, ok := tctx.Runtime.Teams[name]; ok {
				delete(tctx.Runtime.Teams, name)
			}
		}
		tctx.Runtime.Unlock()
	}
	if removed == 0 {
		return Result{Data: fmt.Sprintf("Team '%s' not found in runtime.", name), IsError: true}, nil
	}
	return Result{Data: fmt.Sprintf("Team '%s' removed from runtime (%d members cleaned up).", name, removed)}, nil
}
func (t *TeamDeleteTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("team deletion is safe in current runtime")
}

func NewCronTool() Tool {
	return &CronTool{baseTool{def: Def{
		Name: "cron", Description: "Schedule a recurring task using cron syntax.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"schedule":{"type":"string"},"task":{"type":"string"}},"required":["schedule","task"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, UserFacingName: "Cron",
	}}}
}
func (t *CronTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	schedule, _ := input["schedule"].(string)
	task, _ := input["task"].(string)
	if strings.TrimSpace(schedule) == "" || strings.TrimSpace(task) == "" {
		return Result{Data: "schedule and task are required", IsError: true}, nil
	}
	if !looksLikeCronSchedule(schedule) {
		return Result{Data: fmt.Sprintf("invalid cron schedule %q: expected 5 fields or @daily/@hourly/@weekly/@monthly", schedule), IsError: true}, nil
	}
	if tctx.Runtime == nil {
		return Result{Data: "Cron runtime unavailable; schedule was not registered.", IsError: true}, nil
	}
	tctx.Runtime.Lock()
	ensureRuntimeMaps(tctx.Runtime)
	tctx.Runtime.TaskCounter++
	id := fmt.Sprintf("cron-%d", tctx.Runtime.TaskCounter)
	now := time.Now().Format(time.RFC3339)
	tctx.Runtime.CronSchedules[id] = &CronRecord{ID: id, Schedule: schedule, Task: task, Status: "scheduled", CreatedAt: now}
	tctx.Runtime.Tasks[id] = &TaskRecord{
		ID:          id,
		Title:       fmt.Sprintf("cron %s", schedule),
		Description: task,
		Status:      "scheduled",
		Kind:        "cron",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	tctx.Runtime.Unlock()
	return Result{Data: fmt.Sprintf("Registered local cron schedule [%s] on %s. The current CLI records and tracks the schedule; it does not run detached after process exit.", id, schedule)}, nil
}
func (t *CronTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("cron scheduling requires confirmation")
}

func NewSendMessageTool() Tool {
	return &SendMessageTool{baseTool{def: Def{
		Name: "send_message", Description: "Send a message to another agent or the user.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"message":{"type":"string"}},"required":["to","message"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, UserFacingName: "Send Message",
	}}}
}
func (t *SendMessageTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	to, _ := input["to"].(string)
	msg, _ := input["message"].(string)
	if strings.TrimSpace(to) == "" || strings.TrimSpace(msg) == "" {
		return Result{Data: "to and message are required", IsError: true}, nil
	}
	if tctx.Runtime != nil {
		tctx.Runtime.Lock()
		ensureRuntimeMaps(tctx.Runtime)
		now := time.Now().Format(time.RFC3339)

		if tr, ok := tctx.Runtime.Tasks[to]; ok {
			pending := tr.Status == "pending" || tr.Status == "scheduled"
			tctx.Runtime.Messages = append(tctx.Runtime.Messages, MessageRecord{To: to, Message: msg, CreatedAt: now, Delivered: !pending})
			if tr.Output != "" {
				tr.Output += "\n"
			}
			tr.Output += fmt.Sprintf("message: %s", msg)
			tr.UpdatedAt = now
			tctx.Runtime.Unlock()
			if pending {
				return Result{Data: fmt.Sprintf("Message queued for task %s; it will be delivered into the agent's prompt when the task runs.", to)}, nil
			}
			return Result{Data: fmt.Sprintf("Delivered message to task %s", to)}, nil
		}

		if team, ok := tctx.Runtime.Teams[to]; ok {
			queued := 0
			tctx.Runtime.Messages = append(tctx.Runtime.Messages, MessageRecord{To: to, Message: msg, CreatedAt: now, Delivered: false})
			for _, member := range team.Members {
				if tr, exists := tctx.Runtime.Tasks[member.ID]; exists {
					if tr.Status == "pending" || tr.Status == "scheduled" {
						queued++
					}
					if tr.Output != "" {
						tr.Output += "\n"
					}
					tr.Output += fmt.Sprintf("team message: %s", msg)
					tr.UpdatedAt = now
				}
			}
			tctx.Runtime.Unlock()
			return Result{Data: fmt.Sprintf("Broadcast message to team %s (%d members, %d pending will receive it on start)", to, len(team.Members), queued)}, nil
		}

		if to == "user" {
			tctx.Runtime.Messages = append(tctx.Runtime.Messages, MessageRecord{To: to, Message: msg, CreatedAt: now, Delivered: true})
			tctx.Runtime.Unlock()
			return Result{Data: "Queued message for user in local runtime"}, nil
		}
		tctx.Runtime.Unlock()
	}
	return Result{Data: fmt.Sprintf("Unable to deliver message to %s", to), IsError: true}, nil
}
func (t *SendMessageTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("messaging is safe")
}

func NewLSPTool() Tool {
	return &LSPTool{baseTool{def: Def{
		Name: "lsp", Description: "Language Server Protocol: diagnostics, hover, references, definitions.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"filePath":{"type":"string"}},"required":["action","filePath"]}`),
		IsReadOnly:  true, IsConcurrencySafe: true, UserFacingName: "LSP",
	}}}
}
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
	if strings.EqualFold(action, "diagnostics") {
		return runLocalDiagnostics(ctx, filePath, tctx)
	}
	return Result{Data: fmt.Sprintf("LSP backend unavailable for %s on %s", action, filePath), IsError: true, ShouldRetry: true}, nil
}
func (t *LSPTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("lsp is read-only")
}

func looksLikeCronSchedule(schedule string) bool {
	schedule = strings.TrimSpace(schedule)
	switch schedule {
	case "@hourly", "@daily", "@weekly", "@monthly":
		return true
	}
	return len(strings.Fields(schedule)) == 5
}

func runLocalDiagnostics(ctx context.Context, filePath string, tctx Context) (Result, error) {
	if strings.TrimSpace(filePath) == "" {
		return Result{Data: "filePath is required", IsError: true}, nil
	}
	if !filepath.IsAbs(filePath) && tctx.Cwd != "" {
		filePath = filepath.Join(tctx.Cwd, filePath)
	}
	filePath = filepath.Clean(filePath)
	if _, err := os.Stat(filePath); err != nil {
		return Result{Data: fmt.Sprintf("diagnostics unavailable: %v", err), IsError: true}, nil
	}
	if filepath.Ext(filePath) != ".go" {
		return Result{Data: fmt.Sprintf("No LSP backend configured. Basic diagnostics checked file existence only: %s", filePath)}, nil
	}
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(execCtx, "go", "test", "./...")
	cmd.Dir = filepath.Dir(filePath)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return Result{Data: fmt.Sprintf("Go diagnostics for %s failed:\n%s", filePath, text), IsError: true}, nil
	}
	if text == "" {
		text = "go test ./... passed"
	}
	return Result{Data: fmt.Sprintf("Go diagnostics for %s:\n%s", filePath, text)}, nil
}
