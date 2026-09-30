package dream

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A dream run lasts up to dreamRunTimeout while turn-end extraction keeps
// appending to the same memory files (in this process, or in the session
// process when dream runs as a detached worker). The write tool replaces a
// whole file with what the model composed from the copy it read earlier, so
// a fact extracted in between used to be lost. The runner therefore keeps,
// per memory file, a hash of the content the model last saw (the snapshot
// taken when the run starts, then every read and every write of its own),
// and refuses a write over a file whose content has moved on: the model is
// told to read it again and redo the write on the current content.

// observedKey normalizes a path for the observed map (case-insensitive on
// Windows, where the model may spell the same path differently).
func observedKey(path string) string {
	p := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func contentHash(data []byte) [32]byte { return sha256.Sum256(data) }

// snapshotMemory records every file in the memory roots as the model's
// starting view. Called when a run starts; before it (a runner used outside
// a run) nothing is tracked and writes are not checked.
func (r *Runner) snapshotMemory() {
	seen := map[string][32]byte{}
	for _, root := range r.memoryRoots() {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if data, err := os.ReadFile(p); err == nil {
				seen[observedKey(p)] = contentHash(data)
			}
			return nil
		})
	}
	r.obsMu.Lock()
	r.observed = seen
	r.obsMu.Unlock()
}

// observe records data as the content the model has seen for path.
func (r *Runner) observe(path string, data []byte) {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	if r.observed == nil {
		return
	}
	r.observed[observedKey(path)] = contentHash(data)
}

// staleSinceObserved reports whether current (the file's content now, nil
// when it does not exist) is not what the model last saw of path: changed,
// or created since the run started. Always false outside a run.
func (r *Runner) staleSinceObserved(path string, current []byte, exists bool) bool {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	if r.observed == nil || !exists {
		return false
	}
	h, ok := r.observed[observedKey(path)]
	return !ok || h != contentHash(current)
}
