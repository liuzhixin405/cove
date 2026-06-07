package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type BashTool struct{}

func NewBashTool() Tool {
	def := Def{
		Name: "bash", Description: "Execute shell commands. Use for terminal operations like git, npm, go, docker, tests.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"The command to execute"},
				"description":{"type":"string","description":"Brief description of what this command does"},
				"timeout":{"type":"integer","description":"Optional timeout in milliseconds"}
			},
			"required":["command"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Bash",
	}
	return &bashTool{baseTool{def: def}}
}

type bashTool struct{ baseTool }

func (t *bashTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	cmdStr, _ := input["command"].(string)
	if cmdStr == "" {
		return Result{Data: "Error: command is required", IsError: true}, nil
	}

	timeout := 120 * time.Second
	if ms, ok := input["timeout"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, shellFlag := detectShell()
	cwd := tctx.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	cwd = filepath.Clean(cwd)

	cmd := exec.CommandContext(execCtx, shell, shellFlag, cmdStr)
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
			out = out[:30000] + fmt.Sprintf("\n... [truncated %d bytes, use grep for targeted search]", stdout.Len()-30000)
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

func (t *bashTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	switch tctx.PermissionMode {
	case "bypass", "auto":
		return Allowed("mode: " + tctx.PermissionMode)
	case "plan":
		return Denied("plan mode: bash not allowed")
	}
	return Asked("bash requires approval")
}

func detectShell() (string, string) {
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash", "-c"
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh", "-Command"
	}
	return "cmd", "/C"
}
