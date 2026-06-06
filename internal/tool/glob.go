package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type GlobTool struct{ baseTool }

func NewGlobTool() Tool {
	return &GlobTool{baseTool{def: Def{
		Name: "glob", Description: "Find files matching glob patterns. Support ** for recursive matching.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"pattern":{"type":"string","description":"Glob pattern (e.g. **/*.go, src/**/*.ts)"},
				"path":{"type":"string","description":"Directory to search in (defaults to cwd)"}
			},
			"required":["pattern"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Glob",
	}}}
}

func (t *GlobTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	pattern, _ := input["pattern"].(string)
	basePath, _ := input["path"].(string)
	if basePath == "" {
		basePath = tctx.Cwd
	}
	if basePath == "" {
		basePath = "."
	}

	var matches []string
	err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := filepath.Base(path)
			// Skip common large/irrelevant directories
			switch name {
			case ".git", "node_modules", ".next", ".nuxt", "vendor", "dist", "__pycache__", ".venv", "venv", ".tox":
				return filepath.SkipDir
			}
			if strings.HasPrefix(name, ".") && path != basePath {
				return filepath.SkipDir
			}
		}
		rel, _ := filepath.Rel(basePath, path)
		match, _ := filepath.Match(pattern, filepath.Base(path))
		if !match {
			match, _ = filepath.Match(pattern, rel)
		}
		if !match && strings.HasPrefix(pattern, "**/") {
			match, _ = filepath.Match(strings.TrimPrefix(pattern, "**/"), filepath.Base(path))
		}
		if match {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	if len(matches) == 0 {
		return Result{Data: "No files found for: " + pattern}, nil
	}

	limit := 200
	if len(matches) > limit {
		return Result{Data: strings.Join(matches[:limit], "\n") + "\n... and " + strconv.Itoa(len(matches)-limit) + " more files"}, nil
	}
	return Result{Data: strings.Join(matches, "\n")}, nil
}

func (t *GlobTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("glob is read-only")
}
