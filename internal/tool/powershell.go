package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type PowerShellTool struct{ baseTool }

func NewPowerShellTool() Tool {
	return &PowerShellTool{baseTool{def: Def{
		Name: "powershell", Aliases: []string{"PowerShell"},
		Description: "Execute PowerShell commands. Use for Windows-native operations, .NET calls, and advanced scripting.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"The PowerShell command or script to execute"},
				"description":{"type":"string","description":"Brief description of what this command does"},
				"timeout":{"type":"integer","description":"Optional timeout in milliseconds"}
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

	timeout := 120 * time.Second
	if ms, ok := input["timeout"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Prefer pwsh (PowerShell 7+) over powershell.exe (Windows PowerShell 5.1)
	shell := findPowerShell()

	cwd := tctx.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd = filepath.Clean(cwd)

	// Use -NoProfile to avoid slow startup, -NonInteractive to prevent prompts
	cmd := exec.CommandContext(execCtx, shell, "-NoProfile", "-NonInteractive", "-Command", cmdStr)
	cmd.Dir = cwd
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	exitCode, runErr := streamCommand(execCtx, cmd, &stdout, &stderr, tctx.OnProgress)
	if runErr != nil {
		return Result{Data: fmt.Sprintf("Error: %v\nStderr: %s", runErr, stderr.String()), IsError: true}, nil
	}

	var sb strings.Builder
	if d, ok := input["description"].(string); ok && d != "" {
		sb.WriteString(fmt.Sprintf("Command: %s\n", d))
	}

	if stdout.Len() > 0 {
		out := stdout.String()
		if len(out) > 30000 {
			out = out[:30000] + fmt.Sprintf("\n... [truncated %d bytes]", stdout.Len()-30000)
		}
		sb.WriteString(out)
	}
	if stderr.Len() > 0 {
		sb.WriteString("\n[stderr]\n")
		errOut := stderr.String()
		if len(errOut) > 10000 {
			errOut = errOut[:10000] + fmt.Sprintf("\n... [stderr truncated %d bytes]", stderr.Len()-10000)
		}
		sb.WriteString(errOut)
	}

	if exitCode != 0 {
		sb.WriteString(fmt.Sprintf("\n[exit code: %d]", exitCode))
	}

	return Result{Data: sb.String()}, nil
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
