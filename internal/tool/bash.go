package tool

import (
	"context"
	"encoding/json"

	"github.com/liuzhixin405/cove/internal/shell"
)

func NewBashTool() Tool {
	def := Def{
		Name:        "bash",
		Description: bashDescription(shell.Default()),
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"The command to execute"},
				"description":{"type":"string","description":"Brief description of what this command does"},
				"timeout":{"type":"integer","description":"Optional timeout in milliseconds (default 120000, maximum 600000)"}
			},
			"required":["command"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Bash",
	}
	return &bashTool{baseTool{def: def}}
}

// bashDescription names the interpreter the command really runs in: on
// Windows that may be Git Bash, PowerShell or cmd, and the model has to write
// that shell's syntax.
func bashDescription(sh shell.Shell) string {
	syntax := ""
	switch sh.Kind {
	case shell.PowerShell:
		syntax = " Write PowerShell syntax."
	case shell.Cmd:
		syntax = " Write cmd.exe syntax."
	}
	return "Execute shell commands in " + sh.Describe() + "." + syntax +
		" Use for terminal operations like git, npm, go, docker, tests. " + shellToolLimits
}

type bashTool struct{ baseTool }

func (t *bashTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	cmdStr, _ := input["command"].(string)
	if cmdStr == "" {
		return Result{Data: "Error: command is required", IsError: true}, nil
	}
	return runShell(ctx, shell.Default(), cmdStr, input, tctx), nil
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
