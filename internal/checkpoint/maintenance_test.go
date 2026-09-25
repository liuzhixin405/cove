package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

// gc must not use --prune=now: objects of a checkpoint being written (added,
// not yet referenced) would be deleted, in this process or another one
// sharing the store.
func TestGCDoesNotPruneNow(t *testing.T) {
	for _, a := range gcArgs {
		if strings.Contains(a, "prune=now") {
			t.Fatalf("gc args %q prune unreferenced objects immediately", gcArgs)
		}
	}
}

// gc does not hold the manager lock (without --prune=now it is safe next
// to writes): a Create made while gc runs returns at once.
func TestCreateProceedsWhileGCRuns(t *testing.T) {
	isolatedGit(t)
	oldEvery, oldRun := gcEvery, runGC
	started, release := make(chan struct{}), make(chan struct{})
	gcEvery = 1
	var startOnce sync.Once
	runGC = func(string) { startOnce.Do(func() { close(started) }); <-release }
	t.Cleanup(func() { gcEvery, runGC = oldEvery, oldRun })

	dir := t.TempDir()
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.WaitMaintenance)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	writeFile(t, filepath.Join(dir, "a.txt"), "1")
	if _, err := mgr.Create("one"); err != nil {
		t.Fatal(err)
	}
	<-started
	writeFile(t, filepath.Join(dir, "a.txt"), "2")
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Create("two")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Create waited for the running gc")
	}
	releaseOnce.Do(func() { close(release) })
	mgr.WaitMaintenance()
}

// TestHelperHangingGC is the stand-in for a git gc that never finishes.
func TestHelperHangingGC(t *testing.T) {
	if os.Getenv("COVE_TEST_HANGING_GC") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(time.Minute)
}

// A gc that overruns gcTimeout is killed and reported with a warning.
func TestGCTimeoutKillsAndWarns(t *testing.T) {
	oldCmd, oldTimeout := gcCommand, gcTimeout
	gcTimeout = 300 * time.Millisecond
	gcCommand = func(ctx context.Context, storeDir string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperHangingGC$")
		cmd.Env = append(os.Environ(), "COVE_TEST_HANGING_GC=1")
		return cmd
	}
	t.Cleanup(func() { gcCommand, gcTimeout = oldCmd, oldTimeout })
	var mu sync.Mutex
	var warns []string
	log.AddSink(func(level log.Level, msg string) {
		if level == log.Warn {
			mu.Lock()
			warns = append(warns, msg)
			mu.Unlock()
		}
	})
	t.Cleanup(log.ClearSinks)

	start := time.Now()
	defaultRunGC(t.TempDir())
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("gc ran %v: the timeout did not stop it", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "gc") {
		t.Fatalf("no warning for the killed gc: %q", warns)
	}
}

// Trimming runs in the background: Create returns while the trim is still
// running (the hook blocks until released).
func TestCreateDoesNotWaitForTrim(t *testing.T) {
	isolatedGit(t)
	oldTrim, oldEvery, oldSlack := trimHook, gcEvery, pruneSlack
	gcEvery, pruneSlack = 0, 1
	entered, release := make(chan struct{}, 1), make(chan struct{})
	trimHook = func(m *Manager) { entered <- struct{}{}; <-release }
	t.Cleanup(func() { trimHook, gcEvery, pruneSlack = oldTrim, oldEvery, oldSlack })

	dir := t.TempDir()
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-in first-out: the background trim finishes before
	// the hook variables are restored.
	t.Cleanup(mgr.WaitMaintenance)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	writeFile(t, filepath.Join(dir, "a.txt"), "x")
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Create("c1")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("trim never started")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Create is waiting for the background trim")
	}
	releaseOnce.Do(func() { close(release) })
	mgr.WaitMaintenance()
}
