package tool

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"path/filepath"
	"sync"
)

// FileTracker remembers which version of each file the model has seen in this
// session, so write and edit can refuse to replace a file the model never read
// or that changed on disk since it did. Without it, a write based on a stale
// view silently discarded whatever the user (or a bash command) had changed in
// the meantime.
//
// A file counts as seen after any successful read of it, full or partial
// (offset/limit), and after cove itself wrote or edited it. Partial reads count
// on purpose: edit only replaces text the model quotes exactly, so the lines it
// did not look at are kept byte for byte, and a large file cannot be read in
// one call at all (2000-line default window), so requiring a full read would
// make big files uneditable. What matters for lost updates is that the model's
// view is of the current version, and the snapshot below always covers the
// whole file, whatever part of it was shown.
//
// The snapshot is the size and SHA-256 of the whole file rather than its mtime.
// An mtime check refused files that a formatter, git checkout or touch had
// rewritten with identical bytes, forcing pointless rereads, and it misses a
// change made within the timestamp granularity (2s on FAT, coarse on some
// network shares). Both write and edit read the current bytes anyway, so
// comparing contents costs one hash.
type FileTracker struct {
	mu   sync.Mutex
	seen map[string]fileSnapshot
}

type fileSnapshot struct {
	size int64
	sum  [sha256.Size]byte
}

// Files returns the session's file tracker, creating it on first use. Sub-agents
// share the Runtime, so a file one of them read counts as seen for all.
func (r *Runtime) Files() *FileTracker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files == nil {
		r.files = &FileTracker{seen: make(map[string]fileSnapshot)}
	}
	return r.files
}

// fileTracker returns the tracker for tctx, or nil when the call has no
// session Runtime (headless paths and many tests); every FileTracker method is
// a no-op on nil, so those callers keep the unguarded behavior.
func fileTracker(tctx Context) *FileTracker {
	if tctx.Runtime == nil {
		return nil
	}
	return tctx.Runtime.Files()
}

// trackerKey canonicalizes path so the model reading "a.go" and editing
// "/abs/dir/a.go" (or the file behind a symlink) hit the same entry.
func trackerKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// Record marks data as the version of path the model now knows.
func (ft *FileTracker) Record(path string, data []byte) {
	ft.record(path, fileSnapshot{size: int64(len(data)), sum: sha256.Sum256(data)})
}

func (ft *FileTracker) record(path string, snap fileSnapshot) {
	if ft == nil {
		return
	}
	key := trackerKey(path)
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.seen[key] = snap
}

// Check returns an error, worded for the model, unless current (the file's
// bytes on disk now) is the version it last read or wrote.
func (ft *FileTracker) Check(path string, current []byte) error {
	if ft == nil {
		return nil
	}
	key := trackerKey(path)
	ft.mu.Lock()
	snap, ok := ft.seen[key]
	ft.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s has not been read in this session. Read it first, so the change is based on its current content", path)
	}
	if snap.size != int64(len(current)) || snap.sum != sha256.Sum256(current) {
		return fmt.Errorf("%s has changed since you last read it (edited by the user or a command). Read it again, so those changes are not lost", path)
	}
	return nil
}

// snapshotWriter hashes the bytes streamed through it, for the read tool,
// which never holds the whole file in memory.
type snapshotWriter struct {
	h hash.Hash
	n int64
}

func newSnapshotWriter() *snapshotWriter { return &snapshotWriter{h: sha256.New()} }

func (w *snapshotWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return w.h.Write(p)
}

func (w *snapshotWriter) snapshot() fileSnapshot {
	snap := fileSnapshot{size: w.n}
	copy(snap.sum[:], w.h.Sum(nil))
	return snap
}
