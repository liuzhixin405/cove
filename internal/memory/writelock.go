package memory

import (
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

// writeMu serializes read-modify-write of memory files within one process.
// Turn-end extraction appends to a memory while a background dream run (up
// to five minutes) may be rewriting the same file from the copy it read
// earlier; each used to write back its own version, so whichever landed
// last silently dropped the other's change. Both now hold this lock from
// the read they build on to the write, and dream additionally refuses to
// replace a file that changed since it last read it.
var writeMu sync.Mutex

// LockWrites takes the process-wide memory write lock and returns its
// release. Hold it across a read of a memory file and the write built on it.
func LockWrites() (unlock func()) {
	writeMu.Lock()
	return writeMu.Unlock
}

// WithWriteLock runs fn holding both memory write locks of the memory
// directory dir: LockWrites (this process's writers) first, then dir's lock
// file (other processes' writers: a dream worker, another cove's turn-end
// extraction), the order every writer takes them in, so two writers can never
// each hold one lock while waiting for the other. A lock file held for the
// whole wait comes back as filelock.ErrTimeout and fn does not run.
//
// /memory add wrote through Save with neither lock, so an extraction in the
// middle of its read -> append -> rename replaced what the command had just
// written (or the command replaced the fact extraction had just appended).
func WithWriteLock(dir string, fn func() error) error {
	unlock := LockWrites()
	defer unlock()
	release, err := filelock.MemoryDir(dir)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
