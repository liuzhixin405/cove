package safepath

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Within reports whether target, resolved against root when relative, lies
// inside root (or is root itself). Both paths are cleaned and then have
// symbolic links and junctions resolved (the deepest part that exists, so a
// new file below a linked directory counts too) before a lexical comparison:
// ".." segments that climb out of root, absolute paths elsewhere, other
// drives, and links inside root pointing outside it are all outside. An
// empty root or target is never inside anything.
//
// It is the one "is this path inside that directory" check. There were
// three, each missing something: the file tools' sandbox resolved links
// with filepath.EvalSymlinks, which since Go 1.23 leaves Windows junctions
// unresolved (a junction in the project pointing elsewhere let write and
// edit out of it), and the memory consolidator's sandbox resolved no links
// at all.
func Within(root, target string) bool {
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
// a link chain is longer than maxLinkHops (or loops), or a link's target
// cannot be placed (see linkTarget).
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
				abs, ok := linkTarget(cur, dest)
				if !ok {
					return "", false
				}
				return resolveLinks(filepath.Clean(filepath.Join(append([]string{abs}, parts[i+1:]...)...)), hops+1)
			}
		}
		cur = next
	}
	return cur, true
}

// linkTarget turns dest, a link's stored target as os.Readlink returns it,
// into an absolute path; dir is the directory holding the link. ok is false
// when where the target lies cannot be known, and the caller must treat the
// path as outside.
//
// On Windows a relative target is not always relative to dir. One starting
// with a single "\" or "/" is relative to the root of the link's drive:
// D:\proj\l -> \Users\me\.ssh is D:\Users\me\.ssh. It used to be joined
// under dir like any relative target, so D:\proj\l\id_rsa counted as inside
// D:\proj. One with a drive but no root ("C:foo") is relative to that
// drive's current directory, which depends on the process following the
// link, so it fails closed.
func linkTarget(dir, dest string) (string, bool) {
	if filepath.IsAbs(dest) {
		return dest, true
	}
	if runtime.GOOS == "windows" {
		if filepath.VolumeName(dest) != "" {
			return "", false
		}
		if dest != "" && (dest[0] == '\\' || dest[0] == '/') {
			return filepath.Join(filepath.VolumeName(dir)+string(filepath.Separator), dest), true
		}
	}
	return filepath.Join(dir, dest), true
}

// isLinkMode reports whether an Lstat mode may be a link os.Readlink can
// follow: a symbolic link, or on Windows a reparse point such as a junction
// (reported as ModeIrregular since Go 1.23).
func isLinkMode(m os.FileMode) bool {
	return m&os.ModeSymlink != 0 || runtime.GOOS == "windows" && m&os.ModeIrregular != 0
}
