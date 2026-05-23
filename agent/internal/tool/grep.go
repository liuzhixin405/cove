package tool

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

type GrepTool struct{ baseTool }

func NewGrepTool() Tool {
	return &GrepTool{baseTool{def: Def{
		Name: "grep", Description: "Fast content search with regex. Uses ripgrep (rg) when available, falls back to find+grep.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"pattern":{"type":"string","description":"Regex pattern to search for"},
				"include":{"type":"string","description":"Glob pattern to filter files (e.g. *.go, *.ts)"},
				"path":{"type":"string","description":"Directory to search in (defaults to cwd)"}
			},
			"required":["pattern"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Grep",
	}}}
}

func (t *GrepTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	pattern, _ := input["pattern"].(string)
	include, _ := input["include"].(string)
	searchPath, _ := input["path"].(string)
	if searchPath == "" {
		searchPath = tctx.Cwd
	}
	if searchPath == "" {
		searchPath = "."
	}

	if rg, err := exec.LookPath("rg"); err == nil {
		args := []string{"-n", "--no-heading", "--color=never", "--max-count=50", pattern, searchPath}
		if include != "" {
			args = append(args[:len(args)-2], append([]string{"-g", include}, args[len(args)-2:]...)...)
		}
		cmd := exec.CommandContext(ctx, rg, args...)
		out, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return Result{Data: "No matches found"}, nil
			}
			return Result{Data: "Error: rg failed: " + err.Error(), IsError: true}, nil
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 50 {
			return Result{Data: strings.Join(lines, "\n") + "\n... " + itoa(len(lines)-50) + " more matches"}, nil
		}
		return Result{Data: strings.TrimSpace(string(out))}, nil
	}

	return Result{Data: "ripgrep (rg) not found. Install: https://github.com/BurntSushi/ripgrep\nPattern: " + pattern}, nil
}

func (t *GrepTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("grep is read-only")
}
