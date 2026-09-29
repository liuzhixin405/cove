package engine

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/render"
)

// What an edit or a write changed, for its tool block: the summary "+12 −3"
// and the unified diff behind "/x <id>". The tools themselves report only
// "Edited X: 1 replacement(s)", so the change could not be seen at all.

// fileDiffMaxBytes: files larger than this are not diffed (generated code,
// data), and the block keeps the tool's own summary.
const fileDiffMaxBytes = 1 << 20

// fileDiffsMax bounds the diffs held for blocks not emitted yet (sub-agent
// calls never emit one).
const fileDiffsMax = 256

// isFileWriteTool reports whether name is a tool whose change is diffed.
func isFileWriteTool(name string) bool { return name == "write" || name == "edit" }

// diffTarget is the absolute path a write or edit call changes, "" when it
// names none.
func diffTarget(input map[string]any, cwd string) string {
	p := firstStr(input, "filePath", "file_path", "path")
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// readForDiff returns path's content, and false when it is too large or not
// text. A missing file reads as empty (a write creating it).
func readForDiff(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", os.IsNotExist(err)
	}
	if info.IsDir() || info.Size() > fileDiffMaxBytes {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.IndexByte(string(data[:min(len(data), 8000)]), 0) >= 0 {
		return "", false
	}
	return string(data), true
}

// recordFileDiff stores the diff of a finished write/edit call under its ID
// for emitToolResult. before is the content read before the call.
func (e *Engine) recordFileDiff(id, path, before string) {
	after, ok := readForDiff(path)
	if !ok {
		return
	}
	d := render.Diff(before, after)
	e.diffMu.Lock()
	defer e.diffMu.Unlock()
	if e.fileDiffs == nil || len(e.fileDiffs) >= fileDiffsMax {
		e.fileDiffs = map[string]render.LineDiff{}
	}
	e.fileDiffs[id] = d
}

// takeFileDiff returns and forgets the diff recorded for call id.
func (e *Engine) takeFileDiff(id string) (render.LineDiff, bool) {
	e.diffMu.Lock()
	defer e.diffMu.Unlock()
	d, ok := e.fileDiffs[id]
	delete(e.fileDiffs, id)
	return d, ok
}

// PreviewFileChange returns the diff a write or edit call would make, for the
// permission prompt; ok is false for other tools or when it cannot be
// computed (the file is too large, the edit's text is not found).
func PreviewFileChange(toolName string, input map[string]any, cwd string) (render.LineDiff, bool) {
	if !isFileWriteTool(toolName) {
		return render.LineDiff{}, false
	}
	path := diffTarget(input, cwd)
	if path == "" {
		return render.LineDiff{}, false
	}
	before, ok := readForDiff(path)
	if !ok {
		return render.LineDiff{}, false
	}
	var after string
	switch toolName {
	case "write":
		c, isStr := input["content"].(string)
		if !isStr {
			return render.LineDiff{}, false
		}
		after = c
	case "edit":
		oldS, _ := input["oldString"].(string)
		newS, _ := input["newString"].(string)
		all, _ := input["replaceAll"].(bool)
		if oldS == "" || !strings.Contains(before, oldS) {
			return render.LineDiff{}, false
		}
		if all {
			after = strings.ReplaceAll(before, oldS, newS)
		} else {
			after = strings.Replace(before, oldS, newS, 1)
		}
	}
	return render.Diff(before, after), true
}
