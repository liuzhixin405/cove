package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func resolvePathInCwd(path string, tctx Context, forWrite bool) (string, error) {
	if !filepath.IsAbs(path) && tctx.Cwd != "" {
		path = filepath.Join(tctx.Cwd, path)
	}
	path = filepath.Clean(path)
	if tctx.Cwd == "" {
		return path, nil
	}

	root, err := filepath.Abs(tctx.Cwd)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if evaluatedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = evaluatedRoot
	}

	checkPath := path
	if evaluated, err := filepath.EvalSymlinks(checkPath); err == nil {
		checkPath = evaluated
	} else if forWrite {
		checkPath = nearestExistingParent(filepath.Dir(path))
	}

	checkAbs, err := filepath.Abs(checkPath)
	if err != nil {
		return "", err
	}
	checkAbs = filepath.Clean(checkAbs)
	rel, err := filepath.Rel(root, checkAbs)
	if err != nil {
		// Cross-drive on Windows: filepath.Rel fails when root and path are on different drives.
		// Fall back to case-insensitive path comparison.
		volRoot := filepath.VolumeName(root)
		volPath := filepath.VolumeName(checkAbs)
		if volRoot != "" && volPath != "" && !strings.EqualFold(volRoot, volPath) {
			// Cross-drive: check if path is under a known project root via git
			if gitRoot := findGitRoot(checkAbs); gitRoot != "" {
				gitLower := strings.ToLower(filepath.Clean(gitRoot))
				pathLower := strings.ToLower(filepath.Clean(checkAbs))
				if strings.HasPrefix(pathLower, gitLower+string(os.PathSeparator)) || pathLower == gitLower {
					return path, nil
				}
			}
			return "", fmt.Errorf("path on different drive: %s (cwd is on %s)", path, volRoot)
		}
		// Same drive but Rel still failed — try case-insensitive prefix matching
		lowerRoot := strings.ToLower(filepath.Clean(root))
		lowerPath := strings.ToLower(filepath.Clean(checkAbs))
		if strings.HasPrefix(lowerPath, lowerRoot+string(os.PathSeparator)) || lowerPath == lowerRoot {
			return path, nil
		}
		return "", fmt.Errorf("path outside working directory: %s", path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path outside working directory: %s", path)
	}
	return path, nil
}

func nearestExistingParent(path string) string {
	path = filepath.Clean(path)
	for {
		if _, err := os.Stat(path); err == nil {
			if evaluated, err := filepath.EvalSymlinks(path); err == nil {
				return evaluated
			}
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}


func findGitRoot(path string) string {
	dir := filepath.Dir(path)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
