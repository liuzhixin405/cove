package session

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// NormalizeProjectDir turns dir into the canonical form stored in Record.Cwd:
// absolute and cleaned, so "proj", "./proj" and "proj/" all name one project.
// An empty dir stays empty — it means "unknown", not the current directory.
func NormalizeProjectDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

// SameProjectDir reports whether a and b name the same project directory.
// Windows paths are case-insensitive, and the same folder is routinely spelled
// both "D:\Proj" and "d:\proj" (shell vs. IDE launch), so compare folded there.
// An empty side never matches: a session with no recorded directory belongs to
// no known project.
func SameProjectDir(a, b string) bool {
	a, b = NormalizeProjectDir(a), NormalizeProjectDir(b)
	if a == "" || b == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// FilterByProject keeps the records whose session was started in dir.
//
// Legacy records (saved before Cwd existed) are excluded rather than treated
// as matching: every session on an upgraded machine is legacy, so matching
// them would keep showing other projects' conversations in each project's
// list — the exact confusion this filter exists to remove. They are not lost:
// Store.List still returns them, and the "all" views show them.
func FilterByProject(records []Record, dir string) []Record {
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if SameProjectDir(r.Cwd, dir) {
			out = append(out, r)
		}
	}
	return out
}

// ProjectMismatchWarning returns a user-facing warning when r was started in
// a directory other than cwd, or "" when it is the same project. Resuming
// another codebase's conversation still works (the user may mean it), but the
// file paths in it will not match what the model sees now. Legacy records
// have no directory to compare, so they get no warning.
func ProjectMismatchWarning(r *Record, cwd string) string {
	if r == nil || r.Cwd == "" || SameProjectDir(r.Cwd, cwd) {
		return ""
	}
	return fmt.Sprintf("⚠ 该会话属于其他项目目录: %s\n  当前目录: %s\n  对话中提到的文件路径可能与当前项目不符。\n", r.Cwd, NormalizeProjectDir(cwd))
}
