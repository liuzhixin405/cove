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

	"github.com/liuzhixin405/cove/internal/shell"
	"github.com/liuzhixin405/cove/internal/textutil"
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
	ps := shell.Shell{Kind: shell.PowerShell, Path: findPowerShell()}

	cwd := tctx.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd = filepath.Clean(cwd)

	cmd := exec.CommandContext(execCtx, ps.Path, ps.Args(cmdStr)...)
	cmd.Dir = cwd
	cmd.Env = shell.Env(os.Environ())

	var stdout, stderr bytes.Buffer
	exitCode, runErr := streamCommand(execCtx, cmd, &stdout, &stderr, tctx.OnProgress)
	if runErr != nil {
		return Result{Data: fmt.Sprintf("Error: %v\nStderr: %s", runErr, stderr.String()), IsError: true}, nil
	}

	var sb strings.Builder
	if d, ok := input["description"].(string); ok && d != "" {
		fmt.Fprintf(&sb, "Command: %s\n", d)
	}

	// Clip on a rune boundary and keep both ends: the error lines and exit
	// status are at the bottom.
	if stdout.Len() > 0 {
		sb.WriteString(textutil.ClipMiddleBytes(stdout.String(), 30000))
	}
	if stderr.Len() > 0 {
		sb.WriteString("\n[stderr]\n")
		sb.WriteString(textutil.ClipMiddleBytes(stderr.String(), 10000))
	}

	if exitCode != 0 {
		fmt.Fprintf(&sb, "\n[exit code: %d]", exitCode)
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
