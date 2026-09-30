package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
	"github.com/liuzhixin405/cove-agent/internal/memory"
)

// memoryAddWaitsForLock runs /memory add with args while dir's memory lock
// is held by "another process" and checks the write only lands once the
// lock is released.
func memoryAddWaitsForLock(t *testing.T, store *memory.Store, dir, name string, args ...string) Output {
	t.Helper()
	release, err := filelock.MemoryDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	done := make(chan Output, 1)
	go func() {
		out, err := NewMemoryCmd().Execute(context.Background(), Input{Args: append([]string{"add", name}, args...), MemoryStore: store})
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	select {
	case out := <-done:
		t.Fatalf("/memory add wrote while another process held the memory lock: %q", out.Message)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("memory written while the lock was held (stat err %v)", err)
	}
	release()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("/memory add did not finish after the lock was released")
	}
	return Output{}
}

// /memory add wrote through memory.Store.Save with neither memory write
// lock, so a turn-end extraction (or a dream worker) in the middle of its
// own read -> append -> rename replaced the file the command had just
// written, or the command replaced the fact extraction had just appended.
// The command now takes the same locks, in the same order, as every other
// writer.
func TestMemoryAddHoldsMemoryLocks(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewStoreForDirs(dir)
	out := memoryAddWaitsForLock(t, store, dir, "prefs.md", "Uses", "pnpm.")
	if !strings.Contains(out.Message, "已保存") {
		t.Fatalf("output = %q", out.Message)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "prefs.md")); string(data) != "Uses pnpm." {
		t.Fatalf("memory = %q", data)
	}
}

// The append-over-global path (a name only the global directory has) takes
// the locks too.
func TestMemoryAddOverGlobalHoldsMemoryLocks(t *testing.T) {
	proj, glob := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(glob, "prefs.md"), []byte("Prefers tabs."), 0o644); err != nil {
		t.Fatal(err)
	}
	store := memory.NewStoreForProject(proj, glob)
	out := memoryAddWaitsForLock(t, store, proj, "prefs.md", "Uses", "pnpm.")
	if !strings.Contains(out.Message, "已保存") {
		t.Fatalf("output = %q", out.Message)
	}
	data, _ := os.ReadFile(filepath.Join(proj, "prefs.md"))
	if !strings.Contains(string(data), "Prefers tabs.") || !strings.Contains(string(data), "Uses pnpm.") {
		t.Fatalf("project copy = %q", data)
	}
}
