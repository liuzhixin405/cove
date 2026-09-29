package tool

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safepath"
)

// outsideWorkingDirectoryError is the refusal for a path outside root. The
// first line is what the engine parses (window_shrink.go, outsideDirRe) to
// tell the user; the second tells the model what to do instead of trying
// the same path through bash or powershell, which is what a real session
// spent 90 minutes on.
func outsideWorkingDirectoryError(path, root string) error {
	return fmt.Errorf("path outside working directory: %s\nThe file tools only work inside the working directory %s. Do not work around this with shell commands: tell the user the path is outside the working directory and that they can run /cd <directory> (or start cove there) to work on it", path, root)
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
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if volRoot, volPath := filepath.VolumeName(root), filepath.VolumeName(absPath); volRoot != "" && volPath != "" && !strings.EqualFold(volRoot, volPath) {
		// Another drive is always outside the working directory. It used to
		// fall back to "allow if the target sits inside any git repository",
		// which defeated the sandbox: with cwd on D:, any repository on C:
		// was writable.
		return "", fmt.Errorf("path on different drive: %s (cwd is on %s)\nThe file tools only work inside the working directory %s. Do not work around this with shell commands: tell the user the path is outside the working directory and that they can run /cd <directory> (or start cove there) to work on it", path, volRoot, root)
	}
	// Links and junctions are resolved component by component, so a path
	// through a junction inside the project that points elsewhere is
	// outside. filepath.EvalSymlinks, used here before, leaves junctions
	// unresolved since Go 1.23.
	if !safepath.Within(root, absPath) {
		return "", outsideWorkingDirectoryError(path, root)
	}
	return path, nil
}
