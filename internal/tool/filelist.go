package tool

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safepath"
)

// lstat is os.Lstat, swapped in tests to count the stats a search makes.
var lstat = os.Lstat

// skippedDirs are never listed when walking a directory that is not a git
// repository: version-control internals, dependency caches, build output and
// IDE state. Other dot-directories (.github, .vscode, .cove) are real project
// content and stay visible.
var skippedDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "bower_components": true,
	".venv": true, "venv": true, "__pycache__": true, ".tox": true,
	".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
	".next": true, ".nuxt": true, "dist": true, ".gradle": true,
	".idea": true, ".vs": true, ".cache": true, ".terraform": true,
}

// projectFiles lists the files under base as slash-separated paths relative to
// it. Inside a git repository the list comes from git, so .gitignore applies
// (bin/, obj/, target/ and generated files stay out of search results);
// elsewhere the directory is walked, skipping skippedDirs.
func projectFiles(ctx context.Context, base string) ([]string, error) {
	if files, ok := gitFiles(ctx, base); ok {
		return files, nil
	}
	var files []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != base && skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, err := filepath.Rel(base, path); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

// fileConfiner decides, one entry at a time, whether a listed file (relative
// to base, as projectFiles returns it) exists and really lies inside root, the
// working directory. Both listings return links as entries, and grep's
// built-in search opened them with os.Open, which follows them: a project file
// notes.txt -> ../../.ssh/id_rsa printed the key while read refused the same
// path. Each entry's directory is checked once (a junction there leads out
// too) and the entry itself only when it is not a regular file, so an ordinary
// tree costs one Lstat per file. An empty root confines nothing, as in
// resolvePathInCwd.
//
// The check used to run over the whole listing before grep searched anything
// (and git's listing had already stat'ed every file once more), which in a
// large repository on Windows cost seconds per call although the search stops
// after grepMaxLines matches. Callers now ask per entry as they reach it.
type fileConfiner struct {
	root, base string
	dirInside  map[string]bool
}

func newFileConfiner(root, base string) *fileConfiner {
	return &fileConfiner{root: root, base: base, dirInside: map[string]bool{}}
}

// allow reports whether rel exists and may be opened.
func (c *fileConfiner) allow(rel string) bool {
	full := filepath.Join(c.base, filepath.FromSlash(rel))
	if c.root != "" {
		dir := filepath.Dir(full)
		in, seen := c.dirInside[dir]
		if !seen {
			in = safepath.Within(c.root, dir)
			c.dirInside[dir] = in
		}
		if !in {
			return false
		}
	}
	// Also drops what git ls-files --cached lists after it was deleted from
	// the work tree.
	fi, err := lstat(full)
	if err != nil {
		return false
	}
	return c.root == "" || fi.Mode().IsRegular() || safepath.Within(c.root, full)
}

// confineFiles drops the entries of rels that fileConfiner does not allow.
// An empty root returns rels unchanged.
func confineFiles(root, base string, rels []string) []string {
	if root == "" {
		return rels
	}
	c := newFileConfiner(root, base)
	kept := rels[:0:0]
	for _, rel := range rels {
		if c.allow(rel) {
			kept = append(kept, rel)
		}
	}
	return kept
}

// gitFiles returns tracked and untracked-but-not-ignored files under base, or
// ok=false when base is not inside a git work tree. Files deleted from the
// work tree are still listed (--cached); fileConfiner.allow drops them when
// they are reached.
func gitFiles(ctx context.Context, base string) ([]string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", base, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, entry := range bytes.Split(out, []byte{0}) {
		rel := string(entry)
		if rel == "" || strings.HasPrefix(rel, "../") {
			continue
		}
		files = append(files, rel)
	}
	return files, true
}
