package tool

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

// gitFiles returns tracked and untracked-but-not-ignored files under base, or
// ok=false when base is not inside a git work tree.
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
		// --cached still lists files deleted from the work tree.
		if _, err := os.Lstat(filepath.Join(base, filepath.FromSlash(rel))); err == nil {
			files = append(files, rel)
		}
	}
	return files, true
}
