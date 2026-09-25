package tool

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"

	"github.com/liuzhixin405/cove/internal/shell"
)

type PowerShellTool struct{ baseTool }

func NewPowerShellTool() Tool {
	return &PowerShellTool{baseTool{def: Def{
		Name: "powershell", Aliases: []string{"PowerShell"},
		Description: "Execute PowerShell commands (" + findPowerShell() + "). Use for Windows-native operations, .NET calls, and advanced scripting. Output is UTF-8. " + shellToolLimits,
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"The PowerShell command or script to execute"},
				"description":{"type":"string","description":"Brief description of what this command does"},
				"timeout":{"type":"integer","description":"Optional timeout in milliseconds (default 120000, maximum 600000)"}
			},
			"required":["command"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "PowerShell",
	}}}
}

func (t *PowerShellTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	cmdStr, _ := input["command"].(string)
	if cmdStr == "" {
		return Result{Data: "Error: command is required", IsError: true}, nil
	}

	if runtime.GOOS != "windows" {
		return Result{Data: "Error: PowerShell tool is only available on Windows. Use bash instead.", IsError: true}, nil
	}

	// Prefer pwsh (PowerShell 7+) over powershell.exe (Windows PowerShell 5.1)
	ps := shell.Shell{Kind: shell.PowerShell, Path: findPowerShell()}
	return runShell(ctx, ps, cmdStr, input, tctx), nil
}

func (t *PowerShellTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	switch tctx.PermissionMode {
	case "bypass", "auto":
		return Allowed("mode: " + tctx.PermissionMode)
	case "plan":
		return Denied("plan mode: powershell not allowed")
	}
	return Asked("powershell requires approval")
}

// findPowerShell returns the best available PowerShell executable.
func findPowerShell() string {
	// Prefer pwsh (cross-platform PowerShell 7+)
	if path, err := exec.LookPath("pwsh"); err == nil {
		return path
	}
	// Fallback to Windows PowerShell 5.1
	if path, err := exec.LookPath("powershell"); err == nil {
		return path
	}
	return "powershell.exe"
}
