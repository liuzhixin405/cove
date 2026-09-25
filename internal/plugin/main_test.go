package plugin

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMain removes the template repositories newPluginRepo builds once per
// test binary.
func TestMain(m *testing.M) {
	code := m.Run()
	pluginRepoMu.Lock()
	for _, tmpl := range pluginRepoTemplates {
		root := filepath.Dir(tmpl)
		// git writes its object files read-only, which RemoveAll cannot
		// delete on Windows.
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
		_ = os.RemoveAll(root)
	}
	pluginRepoMu.Unlock()
	os.Exit(code)
}
