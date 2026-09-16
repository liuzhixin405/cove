package tool

import (
	"context"
	"encoding/json"
	"os"
	"path"
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

// matchGlob reports whether relPath matches pattern, with `**` crossing
// directory separators.
//
// filepath.Match alone cannot do this: its `*` never crosses a separator and it
// has no notion of `**` at all, so the documented `src/**/*.ts` form silently
// matched nothing. A leading `**/` was special-cased against the basename,
// which is why only that one form appeared to work.
//
// Semantics, matching the common doublestar convention:
//   - `**` matches zero or more path segments, so `src/**/*.ts` matches both
//     `src/a.ts` and `src/a/b/c.ts`.
//   - a bare `*.go` (no separator in the pattern) matches on the basename, so
//     the shorthand people actually type keeps working.
//   - anything else is matched segment-by-segment via filepath.Match, so `?`,
//     `*` and character classes behave as before within a segment.
func matchGlob(pattern, relPath string) bool {
	pattern = filepath.ToSlash(pattern)
	relPath = filepath.ToSlash(relPath)

	if !strings.Contains(pattern, "/") {
		ok, _ := filepath.Match(pattern, path.Base(relPath))
		return ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(relPath, "/"))
}

// matchSegments matches pattern segments against path segments, treating "**"
// as "zero or more segments".
func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trailing "**" swallows whatever is left, including nothing.
			if len(pat) == 1 {
				return true
			}
			// Try consuming 0, 1, 2 … segments here.
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := filepath.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
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
		if matchGlob(pattern, rel) {
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
