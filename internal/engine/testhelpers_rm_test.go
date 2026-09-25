package engine

import (
	"io/fs"
	"os"
	"path/filepath"
)

// removeTree deletes dir, first making its files writable: git writes object
// files read-only, which os.RemoveAll cannot delete on Windows.
func removeTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
