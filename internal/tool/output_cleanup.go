package tool

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

// ToolOutputMaxAge is how long masked tool outputs written to
// ~/.cove/tool-outputs are kept; older files are deleted at startup.
const ToolOutputMaxAge = 7 * 24 * time.Hour

// ToolOutputDir is the directory the engine's masker writes full tool outputs to.
func ToolOutputDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cove", "tool-outputs")
}

// PruneOldToolOutputs deletes regular files in dir last modified more than
// maxAge ago and returns how many it removed. Subdirectories are left alone;
// a missing dir is a no-op.
func PruneOldToolOutputs(dir string, maxAge time.Duration) int {
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("tool-outputs cleanup: %v", err)
		}
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			log.Warnf("tool-outputs cleanup: %v", err)
			continue
		}
		removed++
	}
	return removed
}
