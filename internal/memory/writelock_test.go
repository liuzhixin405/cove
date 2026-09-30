package memory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

// WithWriteLock holds both memory write locks around fn: the in-process
// one and the directory's lock file, so fn waits for another process's
// writer and holds the file off other processes while it runs.
func TestWithWriteLockHoldsBothLocks(t *testing.T) {
	dir := t.TempDir()
	release, err := filelock.MemoryDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- WithWriteLock(dir, func() error {
			// While fn runs the lock file is this holder's.
			if _, err := filelock.Acquire(filepath.Join(dir, filelock.MemoryLockName), 50*time.Millisecond, time.Minute); !errors.Is(err, filelock.ErrTimeout) {
				return errors.New("lock file not held while fn runs")
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("fn ran while another process held the memory lock (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WithWriteLock did not run fn after the lock was released")
	}
	if _, err := os.Stat(filepath.Join(dir, filelock.MemoryLockName)); !os.IsNotExist(err) {
		t.Fatalf("lock file left behind (stat err %v)", err)
	}
}

// fn's error comes back, and the locks are released either way.
func TestWithWriteLockReturnsFnError(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("fn failed")
	if err := WithWriteLock(dir, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if err := WithWriteLock(dir, func() error { return nil }); err != nil {
		t.Fatalf("second WithWriteLock: %v (locks not released?)", err)
	}
}
