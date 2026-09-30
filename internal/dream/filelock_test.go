package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

// holdMemoryFileLock takes the cross-process memory lock of dir the way
// another process (a turn-end extraction elsewhere) would, and returns its
// release.
func holdMemoryFileLock(t *testing.T, dir string) func() {
	t.Helper()
	release, err := filelock.MemoryDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

// waitsForFileLock runs op while dir's memory file lock is held by "another
// process" and checks op only completes once the lock is released.
func waitsForFileLock(t *testing.T, dir string, op func() string) string {
	t.Helper()
	release := holdMemoryFileLock(t, dir)
	done := make(chan string, 1)
	go func() { done <- op() }()
	select {
	case out := <-done:
		t.Fatalf("memory write went ahead while another process held the memory lock: %q", out)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("memory write did not proceed after the lock was released")
	}
	return ""
}

// memory.LockWrites is a process-local mutex: a dream worker process and the
// interactive process's extraction both passed it at once, and the hash
// check then rename was not atomic between them. Dream's write now also
// takes the memory directory's lock file.
func TestDreamWriteTakesCrossProcessLock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "notes.md")
	r := &Runner{memoryRoot: root}
	out := waitsForFileLock(t, root, func() string {
		return r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": target, "content": "fact"}})
	})
	if strings.HasPrefix(out, "Error") {
		t.Fatalf("write = %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, filelock.MemoryLockName)); !os.IsNotExist(err) {
		t.Fatalf("memory lock left behind (stat err %v)", err)
	}
}

func TestDreamEditTakesCrossProcessLock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "notes.md")
	if err := os.WriteFile(target, []byte("old fact"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{memoryRoot: root}
	out := waitsForFileLock(t, root, func() string {
		return r.executeDreamEdit(api.ToolCall{Input: map[string]any{"filePath": target, "oldString": "old", "newString": "new"}})
	})
	if strings.HasPrefix(out, "Error") {
		t.Fatalf("edit = %q", out)
	}
}

func TestStaleMarkerTakesCrossProcessLock(t *testing.T) {
	mem := t.TempDir()
	target := filepath.Join(mem, "arch.md")
	if err := os.WriteFile(target, []byte("The entry is `runGoneFunction`."), 0o644); err != nil {
		t.Fatal(err)
	}
	waitsForFileLock(t, mem, func() string {
		writeStaleMarker(target, "The entry is `runGoneFunction`.", []string{"runGoneFunction"}, time.Now())
		return "marked"
	})
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), staleMarkerPrefix) {
		t.Fatalf("stale marker missing:\n%s", data)
	}
}

// A write whose lock stays held (a stuck holder) is refused with a message
// the model can act on, rather than going ahead unexcluded.
func TestDreamWriteRefusedWhileLockStaysHeld(t *testing.T) {
	root := t.TempDir()
	holdMemoryFileLock(t, root)
	orig := memoryLockWait
	memoryLockWait = 50 * time.Millisecond
	t.Cleanup(func() { memoryLockWait = orig })
	r := &Runner{memoryRoot: root}
	out := r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": filepath.Join(root, "n.md"), "content": "x"}})
	if !strings.HasPrefix(out, "Error") || !strings.Contains(out, "retry") {
		t.Fatalf("write under a held lock = %q, want a retry error", out)
	}
	if _, err := os.Stat(filepath.Join(root, "n.md")); !os.IsNotExist(err) {
		t.Fatal("the write went ahead without the lock")
	}
}
