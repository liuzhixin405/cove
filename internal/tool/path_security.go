package tool

import (
	"fmt"
	"path/filepath"
	"runtime"
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
	// "README.md:notes" is, on NTFS, a hidden alternate data stream of
	// README.md. Only the screenshot and draw_image paths refused it; write
	// and edit created such a stream inside the workspace, invisible to git,
	// Explorer and read. Every file tool resolves through here, so refuse it
	// here.
	if hasStreamSeparator(path, runtime.GOOS) {
		return "", fmt.Errorf("path must not contain ':' after the drive letter (on Windows it names an alternate data stream of another file): %s", path)
	}
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

// hasStreamSeparator reports whether p, on Windows (goos), has a ':' after
// its volume name. NTFS reads "main.go:x.png" as the stream x.png of main.go,
// so a path that passes a ".png" check could write into a source file's
// hidden alternate data stream. ':' is an ordinary name character elsewhere.
func hasStreamSeparator(p, goos string) bool {
	if goos != "windows" {
		return false
	}
	// The extended-length forms are four characters (`\\?\C:\...`); they were
	// listed as three, so a `\\?\` path was never stripped and its drive
	// colon counted as a stream separator.
	for _, prefix := range []string{`\\?\`, `\\.\`, `//?/`, `//./`} {
		if strings.HasPrefix(p, prefix) {
			p = p[len(prefix):]
			break
		}
	}
	if len(p) >= 2 && p[1] == ':' && ('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z') {
		p = p[2:]
	}
	return strings.Contains(p, ":")
}
