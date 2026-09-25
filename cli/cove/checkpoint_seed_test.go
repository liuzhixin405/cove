package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// engine.New opens the checkpoint store under HOME and, in a fresh test HOME,
// creates it with "git init --bare" — a git process costs a few hundred
// milliseconds on Windows, and nearly every test here starts an engine in
// its own HOME. seedCheckpointStore copies one store initialized per test
// binary into home instead; New then finds it and runs no git at all.
var (
	checkpointTemplateOnce sync.Once
	checkpointTemplate     string // "" when git is unavailable
)

// TestMain removes the template store after the run.
func TestMain(m *testing.M) {
	// No test may start a real detached `cove --dream-worker` (os.Executable
	// is the test binary): exit-path tests with two or more turns would.
	// Tests that exercise the spawn replace it themselves (stubDreamSpawn).
	dreamSpawn = func([]string) (int, error) { return 0, nil }
	code := m.Run()
	if checkpointTemplate != "" {
		root := filepath.Dir(checkpointTemplate)
		// git writes some files read-only, which RemoveAll cannot delete on
		// Windows.
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
		_ = os.RemoveAll(root)
	}
	os.Exit(code)
}

func seedCheckpointStore(t *testing.T, home string) {
	t.Helper()
	checkpointTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cove-cli-test-store-")
		if err != nil {
			return
		}
		store := filepath.Join(dir, "store")
		if err := exec.Command("git", "init", "--quiet", "--bare", store).Run(); err != nil {
			return
		}
		checkpointTemplate = store
	})
	if checkpointTemplate == "" {
		return // New initializes the store itself, as before
	}
	dst := filepath.Join(home, ".cove", "checkpoints", "store")
	err := filepath.WalkDir(checkpointTemplate, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(checkpointTemplate, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("seed checkpoint store: %v", err)
	}
}
