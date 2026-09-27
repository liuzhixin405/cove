package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// outsideWorkingDirectoryError is the refusal for a path outside root. The
// first line is what the engine parses (window_shrink.go, outsideDirRe) to
// tell the user; the second tells the model what to do instead of trying
// the same path through bash or powershell, which is what a real session
// spent 90 minutes on.
func outsideWorkingDirectoryError(path, root string) error {
	return fmt.Errorf("path outside working directory: %s\nThe file tools only work inside the working directory %s. Do not work around this with shell commands: tell the user the path is outside the working directory and that they can run /cd <directory> (or start cove there) to work on it.", path, root)
}

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
			// Cross-drive is always outside the working directory.
			//
			// This used to fall back to "allow if the target sits inside any git
			// repository", which defeated the sandbox entirely: with cwd on D:,
			// C:\Users\<me>\anything-with-a-.git\ was writable. A repo somewhere
			// else on the machine is not the cwd, and symlinks/junctions into the
			// cwd were already resolved by EvalSymlinks above, so a legitimate
			// in-project path can never land here.
			return "", fmt.Errorf("path on different drive: %s (cwd is on %s)\nThe file tools only work inside the working directory %s. Do not work around this with shell commands: tell the user the path is outside the working directory and that they can run /cd <directory> (or start cove there) to work on it.", path, volRoot, root)
		}
		// Same drive but Rel still failed; try case-insensitive prefix matching.
		lowerRoot := strings.ToLower(filepath.Clean(root))
		lowerPath := strings.ToLower(filepath.Clean(checkAbs))
		if strings.HasPrefix(lowerPath, lowerRoot+string(os.PathSeparator)) || lowerPath == lowerRoot {
			return path, nil
		}
		return "", outsideWorkingDirectoryError(path, root)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", outsideWorkingDirectoryError(path, root)
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
