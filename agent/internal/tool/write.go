package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type WriteTool struct{ baseTool }

func NewWriteTool() Tool {
	return &WriteTool{baseTool{def: Def{
		Name: "write", Aliases: []string{"Write"},
		Description: "Write a file to the local filesystem. Creates parent directories if needed. Overwrites existing files.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filePath":{"type":"string","description":"Absolute path to write the file"},
				"content":{"type":"string","description":"The content to write"}
			},
			"required":["filePath","content"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Write",
	}}}
}

func (t *WriteTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	path, _ := input["filePath"].(string)
	content, _ := input["content"].(string)
	if path == "" {
		return Result{Data: "Error: filePath required", IsError: true}, nil
	}
	if !filepath.IsAbs(path) && tctx.Cwd != "" {
		path = filepath.Join(tctx.Cwd, path)
	}
	path = filepath.Clean(path)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return Result{Data: "Error: mkdir: " + err.Error(), IsError: true}, nil
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return Result{Data: "Error: write: " + err.Error(), IsError: true}, nil
	}

	lines := len(strings.Split(content, "\n"))
	return Result{Data: "Wrote " + itoa(len(content)) + " bytes (" + itoa(lines) + " lines) to " + path}, nil
}

func (t *WriteTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	switch tctx.PermissionMode {
	case "bypass", "auto":
		return Allowed("mode: " + tctx.PermissionMode)
	case "plan":
		return Denied("plan mode: write not allowed")
	}
	return Asked("write modifies filesystem")
}
