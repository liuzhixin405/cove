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
		return "", err
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
