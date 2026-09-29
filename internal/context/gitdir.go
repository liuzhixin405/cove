package context

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Reading repository facts from .git directly, without starting git.

// findGitDir walks up from cwd to the first directory holding .git and
// returns the git directory (following a worktree's "gitdir:" file) and the
// work tree root; both "" outside a repository.
func findGitDir(cwd string) (gitDir, root string) {
	for dir := filepath.Clean(cwd); ; {
		p := filepath.Join(dir, ".git")
		if info, err := os.Stat(p); err == nil {
			if info.IsDir() {
				return p, dir
			}
			// A worktree or submodule: ".git" is a file naming the git dir.
			if data, err := os.ReadFile(p); err == nil {
				if gd, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:"); ok {
					gd = strings.TrimSpace(gd)
					if !filepath.IsAbs(gd) {
						gd = filepath.Join(dir, gd)
					}
					if st, err := os.Stat(gd); err == nil && st.IsDir() {
						return filepath.Clean(gd), dir
					}
				}
			}
			return "", ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// commonDir is where a git dir's shared refs live: the main repository's
// git dir for a worktree ("commondir" file), else the git dir itself.
func commonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	cd := strings.TrimSpace(string(data))
	if !filepath.IsAbs(cd) {
		cd = filepath.Join(gitDir, cd)
	}
	return filepath.Clean(cd)
}

// headBranch is what "git rev-parse --abbrev-ref HEAD" prints: the branch
// HEAD points at, or "HEAD" when it is detached.
func headBranch(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(head, "ref:"); ok {
		return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	}
	return "HEAD"
}

// hasRef reports whether ref ("refs/heads/main") exists, loose or packed.
func hasRef(common, ref string) bool {
	if _, err := os.Stat(filepath.Join(common, filepath.FromSlash(ref))); err == nil {
		return true
	}
	f, err := os.Open(filepath.Join(common, "packed-refs"))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); strings.HasSuffix(line, " "+ref) {
			return true
		}
	}
	return false
}

// mainBranch is detectMainBranch from the files: main or master when the
// branch exists, else the branch origin/HEAD points at, else "main".
func mainBranch(gitDir string) string {
	common := commonDir(gitDir)
	for _, name := range []string{"main", "master"} {
		if hasRef(common, "refs/heads/"+name) {
			return name
		}
	}
	if data, err := os.ReadFile(filepath.Join(common, "refs", "remotes", "origin", "HEAD")); err == nil {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref:"); ok {
			parts := strings.Split(strings.TrimSpace(ref), "/")
			return parts[len(parts)-1]
		}
	}
	return "main"
}

// branchOf is the current branch of the repository at root, from its files
// when they can be found, else from git.
func branchOf(root string) string {
	if gd, _ := findGitDir(root); gd != "" {
		return headBranch(gd)
	}
	return gitBranch(root)
}
