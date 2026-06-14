package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type SleepTool struct{ baseTool }
type BriefTool struct{ baseTool }

func NewSleepTool() Tool {
	return &SleepTool{baseTool{def: Def{
		Name: "sleep", Description: "Pause agent execution for a duration.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"seconds":{"type":"integer"}},"required":["seconds"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, UserFacingName: "Sleep",
	}}}
}
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
func (t *SleepTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("sleep is safe")
}

func NewBriefTool() Tool {
	return &BriefTool{baseTool{def: Def{
		Name: "brief", Description: "Generate a session or context summary.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"what":{"type":"string"}}}`),
		IsReadOnly:  true, IsConcurrencySafe: true, UserFacingName: "Brief",
	}}}
}
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
func (t *BriefTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("brief is read-only")
}
