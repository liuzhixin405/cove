package extract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

// memory.LockWrites only excludes writers in this process: a dream worker
// (another process) replacing a memory file after its hash check could land
// between an extraction's read and its write, and one of the two lost the
// other's content. The write loop now also holds the memory directory's lock
// file, which dream's writes take too.
func TestExtractWaitsForCrossProcessMemoryLock(t *testing.T) {
	p := &fakeProvider{response: memoryBlock("notes.md", "write", "a durable fact")}
	r, dir := newTestRunner(t, p)
	release, err := filelock.MemoryDir(dir) // "another process" is writing
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	done := make(chan struct{})
	go func() {
		r.Extract(context.Background(), conversation(6))
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); err == nil {
		t.Fatal("extraction wrote while another process held the memory lock")
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("extraction did not finish after the lock was released")
	}
	if got := readMemory(t, dir, "notes.md"); !strings.Contains(got, "a durable fact") {
		t.Fatalf("notes.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, filelock.MemoryLockName)); !os.IsNotExist(err) {
		t.Fatalf("memory lock left behind (stat err %v)", err)
	}
}
