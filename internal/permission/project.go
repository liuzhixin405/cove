package permission

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safepath"
)

// TargetPath returns the file a write/edit call targets, trying the keys the
// write and edit tools accept ("filePath" and the fallbacks models use).
func TargetPath(input map[string]any) string {
	for _, k := range []string{"filePath", "file_path", "path", "filepath", "file"} {
		if s, ok := input[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// PathInside reports whether target, resolved against root when relative,
// lies inside root, links and junctions resolved; see safepath.Within.
func PathInside(root, target string) bool {
	return safepath.Within(root, target)
}

// ProjectRoot is the directory a persistent ("[p]") permission rule is
// scoped to: the nearest ancestor of cwd holding a .git entry, else cwd
// itself, as an absolute cleaned path. It only looks at the file system and
// never runs git.
func ProjectRoot(cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return filepath.Clean(cwd)
	}
	for dir := abs; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		dir = parent
	}
}

// SameProject compares two project roots the way the file system does:
// cleaned, and case-insensitively on Windows.
func SameProject(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
