package permission

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
// lies inside root (or is root itself). Both paths are cleaned and then have
// symbolic links and junctions resolved (resolveExisting: the deepest part
// that exists, so a new file below a linked directory counts too) before a
// lexical comparison: ".." segments that climb out of root, absolute paths
// elsewhere, and links inside root pointing outside it are all outside. An
// empty root is never inside anything.
func PathInside(root, target string) bool {
	if root == "" || target == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(absRoot, target)
	}
	target, ok := resolveExisting(filepath.Clean(target))
	if !ok {
		return false
	}
	absRoot, ok = resolveExisting(absRoot)
	if !ok {
		return false
	}
	rel, err := filepath.Rel(absRoot, target)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// Rel is case-sensitive; the Windows file system is not.
		rel2, err2 := filepath.Rel(strings.ToLower(absRoot), strings.ToLower(target))
		if err2 == nil {
			rel = rel2
		}
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolveExisting resolves symbolic links and, on Windows, junctions in the
// clean absolute path p, component by component, up to the first component
// that does not exist; the rest is appended unchanged, so a file that does
// not exist yet is judged by where its directory really is. ok is false when
// a link chain is longer than maxLinkHops (or loops).
// filepath.EvalSymlinks is not enough: since Go 1.23 it leaves Windows mount
// points (junctions, which mklink /J makes without privileges) unresolved.
func resolveExisting(p string) (string, bool) {
	return resolveLinks(p, 0)
}

// maxLinkHops bounds link chains (and loops) the way the OS does.
const maxLinkHops = 40

// linkMode and readLink are os.Lstat's mode and os.Readlink; variables so a
// test can fake a link loop.
var (
	linkMode = func(p string) (os.FileMode, error) {
		fi, err := os.Lstat(p)
		if err != nil {
			return 0, err
		}
		return fi.Mode(), nil
	}
	readLink = os.Readlink
)

// resolveLinks does the work of resolveExisting. ok is false when a chain
// needs more than maxLinkHops hops: where it ends is unknown, so the caller
// must not treat the path as inside anything.
func resolveLinks(p string, hops int) (string, bool) {
	vol := filepath.VolumeName(p)
	parts := strings.Split(strings.TrimLeft(p[len(vol):], string(filepath.Separator)), string(filepath.Separator))
	cur := vol + string(filepath.Separator)
	for i, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(cur, part)
		mode, err := linkMode(next)
		if err != nil {
			return filepath.Join(append([]string{next}, parts[i+1:]...)...), true
		}
		if isLinkMode(mode) {
			if dest, err := readLink(next); err == nil {
				if hops >= maxLinkHops {
					return "", false
				}
				if !filepath.IsAbs(dest) {
					dest = filepath.Join(cur, dest)
				}
				return resolveLinks(filepath.Clean(filepath.Join(append([]string{dest}, parts[i+1:]...)...)), hops+1)
			}
		}
		cur = next
	}
	return cur, true
}

// isLinkMode reports whether an Lstat mode may be a link os.Readlink can
// follow: a symbolic link, or on Windows a reparse point such as a junction
// (reported as ModeIrregular since Go 1.23).
func isLinkMode(m os.FileMode) bool {
	return m&os.ModeSymlink != 0 || runtime.GOOS == "windows" && m&os.ModeIrregular != 0
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
