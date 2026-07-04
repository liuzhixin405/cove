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
	"unicode/utf8"
)

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
			trimmed, truncated := truncateUTF8(out, 30000)
			out = trimmed + fmt.Sprintf("\n... [truncated %d bytes, use grep for targeted search]", truncated)
		}
		sb.WriteString(out)
	}
	if stderr.Len() > 0 {
		sb.WriteString("\n[stderr]\n")
		errOut := stderr.String()
		if len(errOut) > 10000 {
			trimmed, truncated := truncateUTF8(errOut, 10000)
			errOut = trimmed + fmt.Sprintf("\n... [stderr truncated %d bytes]", truncated)
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

func truncateUTF8(s string, maxBytes int) (string, int) {
	if len(s) <= maxBytes {
		return s, 0
	}

	cut := maxBytes
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	if cut <= 0 {
		return "", len(s)
	}
	return s[:cut], len(s) - cut
}
