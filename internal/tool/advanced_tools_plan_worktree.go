package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type PlanModeTool struct{ baseTool }
type ExitPlanModeTool struct{ baseTool }
type EnterWorktreeTool struct{ baseTool }
type ExitWorktreeTool struct{ baseTool }

func NewPlanModeTool() Tool {
	return &PlanModeTool{baseTool{def: Def{
		Name: "plan_mode", Aliases: []string{"EnterPlanMode"},
		Description: "Enter plan mode: only read operations allowed. Use before complex changes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
		IsReadOnly:  true, UserFacingName: "Plan Mode",
	}}}
}
func (t *PlanModeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	reason, _ := input["reason"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.PlanMode = true
	}
	return Result{Data: fmt.Sprintf("Plan mode active. Read-only operations only. Reason: %s", reason)}, nil
}
func (t *PlanModeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("plan mode entry is safe")
}

func NewExitPlanModeTool() Tool {
	return &ExitPlanModeTool{baseTool{def: Def{
		Name: "exit_plan_mode", Aliases: []string{"ExitPlanMode"},
		Description: "Exit plan mode. Full tool access restored.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
		IsReadOnly:  false, UserFacingName: "Exit Plan Mode",
	}}}
}
func (t *ExitPlanModeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	summary, _ := input["summary"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.PlanMode = false
	}
	return Result{Data: fmt.Sprintf("Plan mode exited. Summary: %s", summary)}, nil
}
func (t *ExitPlanModeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("exiting plan mode requires confirmation")
}

func NewEnterWorktreeTool() Tool {
	return &EnterWorktreeTool{baseTool{def: Def{
		Name: "worktree", Aliases: []string{"EnterWorktree"},
		Description: "Create a git worktree for isolated work.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string"}},"required":["branch"]}`),
		IsReadOnly:  false, UserFacingName: "Enter Worktree",
	}}}
}
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
func (t *EnterWorktreeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("worktree creation requires confirmation")
}

func NewExitWorktreeTool() Tool {
	return &ExitWorktreeTool{baseTool{def: Def{
		Name: "exit_worktree", Aliases: []string{"ExitWorktree"},
		Description: "Exit the current worktree.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"merge":{"type":"boolean"}}}`),
		IsReadOnly:  false, UserFacingName: "Exit Worktree",
	}}}
}
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
func (t *ExitWorktreeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("worktree removal requires confirmation")
}
