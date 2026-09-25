package checkpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives the package one throwaway HOME, so the shadow store is
// created once ("git init --bare") and shared, and hides every git config
// and identity (see isolatedGit). Tests that need other settings set them
// with t.Setenv and run sequentially.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "cove-checkpoint-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkpoint tests:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "no-gitconfig"))
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		os.Unsetenv(k)
	}
	// Create the store up front: parallel tests must not race to init it.
	work := filepath.Join(home, "init-work")
	if err := os.MkdirAll(work, 0o700); err == nil {
		_, _ = New(work)
	}
	code := m.Run()
	removeTree(home)
	os.Exit(code)
}
