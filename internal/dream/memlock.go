package dream

import (
	"path/filepath"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
	"github.com/liuzhixin405/cove-agent/internal/memory"
)

// memoryLockWait is how long a memory write waits for another process's
// memory lock; a variable so tests can shorten it.
var memoryLockWait = filelock.DefaultWait

// lockMemoryDir takes both memory write locks for a read-modify-write of a
// file in the memory directory dir: memory.LockWrites (this process's
// writers) first, then dir's lock file (other processes' writers: a dream
// worker and the interactive process's extraction). LockWrites alone used to
// be all there was, so a worker's "hash check, then rename" and another
// process's append interleaved and one of them lost the other's content.
// The order is the same in every writer (extraction too), so two writers can
// never each hold one lock while waiting for the other.
func lockMemoryDir(dir string) (unlock func(), err error) {
	unlockWrites := memory.LockWrites()
	release, err := filelock.Acquire(filepath.Join(dir, filelock.MemoryLockName), memoryLockWait, filelock.DefaultStale)
	if err != nil {
		unlockWrites()
		return nil, err
	}
	return func() {
		release()
		unlockWrites()
	}, nil
}

// memoryRootOf is the memory root absPath lies in (whose lock guards it);
// absPath's own directory when none matches.
func (r *Runner) memoryRootOf(absPath string) string {
	for _, root := range r.memoryRoots() {
		if isInsideMemoryDir(absPath, root) {
			return root
		}
	}
	return filepath.Dir(absPath)
}

// lockTimeoutMsg is the tool result for a write whose lock stayed held.
func lockTimeoutMsg(filePath string, err error) string {
	return "Error: " + filePath + " is being written by another cove process (" + err.Error() + "); retry the write in a moment"
}
