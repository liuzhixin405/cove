package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type TeamCreateTool struct{ baseTool }
type TeamDeleteTool struct{ baseTool }
type SendMessageTool struct{ baseTool }

func NewTeamCreateTool() Tool {
	return &TeamCreateTool{baseTool{def: Def{
		Name: "team_create", Description: "Create a team of agents for parallel work.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"members":{"type":"array","items":{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"}}}}},"required":["name","members"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, PlanSafe: true, UserFacingName: "Team Create",
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
	fmt.Fprintf(&sb, "Team '%s' created with %d members:\n", name, len(members))
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
			fmt.Fprintf(&sb, "  %d. [%s] %s\n", i+1, ag, tsk)
		}
	}
	// Keep the lock across the summary below: it reads the same maps the loop
	// above writes, and releasing it here would let another goroutine mutate
	// them mid-iteration (a fatal error).
	var planExec func(ctx context.Context, parallel bool) (string, error)
	if tctx.Runtime != nil {
		tctx.Runtime.Teams[name] = &TeamRecord{Name: name, Members: memberRecords, Status: "active", CreatedAt: now}
		if len(tctx.Runtime.Teams) > 0 {
			fmt.Fprintf(&sb, "Teams: %d\n", len(tctx.Runtime.Teams))
			for _, team := range tctx.Runtime.Teams {
				fmt.Fprintf(&sb, "- %s [%s]: %d members\n", team.Name, team.Status, len(team.Members))
			}
		}
		if len(tctx.Runtime.Messages) > 0 {
			fmt.Fprintf(&sb, "Messages: %d queued\n", len(tctx.Runtime.Messages))
		}
		// Capture the callback but do not call it here. It runs the plan
		// executor, which calls plan.FromRuntime, which takes this same lock —
		// and sync.Mutex is not reentrant, so calling it under the lock would
		// deadlock the whole turn.
		planExec = tctx.Runtime.PlanExecuteFunc
		tctx.Runtime.Unlock()
	}

	if planExec != nil {
		result, err := planExec(ctx, true)
		if err != nil {
			fmt.Fprintf(&sb, "\n\n[执行失败] %v", err)
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
		IsReadOnly:  false, PlanSafe: true, UserFacingName: "Team Delete",
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
			delete(tctx.Runtime.Teams, name)
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

func NewSendMessageTool() Tool {
	return &SendMessageTool{baseTool{def: Def{
		Name: "send_message", Description: "Send a message to another agent or the user.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"message":{"type":"string"}},"required":["to","message"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, PlanSafe: true, UserFacingName: "Send Message",
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
