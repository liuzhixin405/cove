package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	// Fallback: models sometimes use alternative key names
	if path == "" {
		for _, alt := range []string{"file_path", "path", "filepath", "file"} {
			if v, ok := input[alt].(string); ok && v != "" {
				path = v
				break
			}
		}
	}
	if content == "" {
		if v, ok := input["text"].(string); ok && v != "" {
			content = v
		}
	}

	if path == "" {
		// Log the full input keys for debugging
		keys := make([]string, 0, len(input))
		for k := range input {
			keys = append(keys, k)
		}
		return Result{Data: fmt.Sprintf("Error: filePath required (received keys: %v)", keys), IsError: true}, nil
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

	lines := strings.Count(content, "\n") + 1
	return Result{Data: "Wrote " + strconv.Itoa(len(content)) + " bytes (" + strconv.Itoa(lines) + " lines) to " + path}, nil
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
