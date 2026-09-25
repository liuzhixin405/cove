package tool

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/repomap"
)

// repoMapToolMaxBytes caps one repo_map result.
const repoMapToolMaxBytes = 12 * 1024

// RepoMapTool answers repo map queries on demand. The system prompt only
// carries a project outline; files, symbols and line numbers come from here.
type RepoMapTool struct{ baseTool }

func NewRepoMapTool() Tool {
	return &RepoMapTool{baseTool{def: Def{
		Name: "repo_map",
		Description: "Show the repository map: source files with their types, functions and methods (signature and line), ranked by relevance. " +
			"query: space-separated path fragments or identifiers (e.g. \"engine turnContextNote\"); matches file paths and symbol names, case-insensitive. " +
			"path: only files under this directory. Both optional; with neither, lists the most referenced files. Output is capped at 12KB. " +
			"Maps Go, Python, TypeScript and JavaScript (.go .py .ts .tsx .js .jsx .mjs .cjs); use grep/glob for other languages.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"query":{"type":"string","description":"Path fragments or identifiers, space separated (optional)"},
				"path":{"type":"string","description":"Directory to restrict the map to, relative to the working directory (optional)"}
			}
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "RepoMap",
	}}}
}

func (t *RepoMapTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	query, _ := input["query"].(string)
	dir, _ := input["path"].(string)
	root := tctx.Cwd
	if root == "" {
		root = "."
	}
	prefix := ""
	if strings.TrimSpace(dir) != "" {
		abs, err := resolvePathInCwd(dir, tctx, false)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		if rel, err := filepath.Rel(root, abs); err == nil {
			prefix = rel
		}
	}
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ',' || r == '|'
	})
	out := repomap.QueryIn(root, prefix, terms, repoMapToolMaxBytes)
	if out == "" {
		return Result{Data: "No files match the query (only .go/.py/.ts/.js source files are mapped); try other terms, grep or glob."}, nil
	}
	return Result{Data: out}, nil
}

func (t *RepoMapTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("repo_map is read-only")
}
