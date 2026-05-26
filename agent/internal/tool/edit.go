package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type EditTool struct{ baseTool }

func NewEditTool() Tool {
	return &EditTool{baseTool{def: Def{
		Name: "edit", Aliases: []string{"Edit"},
		Description: "Make exact string replacements in files. Fails if oldString is not found or matches multiple times (use replaceAll for multiple).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filePath":{"type":"string","description":"Absolute path to the file to edit"},
				"oldString":{"type":"string","description":"The exact text to replace"},
				"newString":{"type":"string","description":"The text to replace with (must differ from oldString)"},
				"replaceAll":{"type":"boolean","description":"Replace all occurrences"}
			},
			"required":["filePath","oldString","newString"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Edit",
	}}}
}

func (t *EditTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	path, _ := input["filePath"].(string)
	oldS, _ := input["oldString"].(string)
	newS, _ := input["newString"].(string)
	all, _ := input["replaceAll"].(bool)

	if path == "" {
		return Result{Data: "Error: filePath required", IsError: true}, nil
	}
	if oldS == newS {
		return Result{Data: "Error: oldString and newString must differ", IsError: true}, nil
	}
	if oldS == "" {
		return Result{Data: "Error: oldString is empty", IsError: true}, nil
	}

	if !filepath.IsAbs(path) && tctx.Cwd != "" {
		path = filepath.Join(tctx.Cwd, path)
	}
	path = filepath.Clean(path)

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Data: "Error: file not found: " + path, IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	content := string(data)
	if strings.Count(content, oldS) == 0 {
		return Result{Data: "Error: oldString not found in file", IsError: true}, nil
	}
	if strings.Count(content, oldS) > 1 && !all {
		return Result{Data: fmt.Sprintf("Error: oldString matches %d times. Use replaceAll=true or provide more context.", strings.Count(content, oldS)), IsError: true}, nil
	}

	var result string
	if all {
		result = strings.ReplaceAll(content, oldS, newS)
	} else {
		result = strings.Replace(content, oldS, newS, 1)
	}

	perm := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(result), perm); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	count := 1
	if all {
		count = strings.Count(content, oldS)
	}
	return Result{Data: fmt.Sprintf("Edited %s: %d replacement(s)", path, count)}, nil
}

func (t *EditTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("edit modifies file content")
}
