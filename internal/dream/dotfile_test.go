package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

// The memory directory's bookkeeping lives in dot files: the consolidation
// lock, its takeover guard, the memory write lock and the extraction record.
// Dream's write and edit only checked that the path lay inside a memory
// root, so a model-chosen filePath of .consolidate-lock replaced the very
// lock this run holds (another process then started a second consolidation
// on the same files). Memories are never dot files (the store skips them),
// so any dot-file path is refused.
func TestDreamWriteAndEditRefuseDotFiles(t *testing.T) {
	root := t.TempDir()
	r := &Runner{memoryRoot: root}
	r.snapshotMemory()
	for _, name := range []string{lockFileName, lockFileName + takeoverSuffix, filelock.MemoryLockName, ".last-extraction.json", ".hidden.md"} {
		target := filepath.Join(root, name)
		if err := os.WriteFile(target, []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": target, "content": "done 1"}})
		if !strings.HasPrefix(out, "Error") {
			t.Errorf("write to %s accepted: %q", name, out)
		}
		out = r.executeDreamEdit(api.ToolCall{Input: map[string]any{"filePath": target, "oldString": "12345", "newString": "done 1"}})
		if !strings.HasPrefix(out, "Error") {
			t.Errorf("edit of %s accepted: %q", name, out)
		}
		if data, _ := os.ReadFile(target); string(data) != "12345" {
			t.Errorf("%s changed to %q", name, data)
		}
		// The stand-in for the memory write lock would otherwise be waited
		// for by the plain write below.
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
	}
	// A dot file that does not exist yet is not created either.
	target := filepath.Join(root, ".new-lock")
	if out := r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": target, "content": "x"}}); !strings.HasPrefix(out, "Error") {
		t.Errorf("write creating a dot file accepted: %q", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("dot file created (stat err %v)", err)
	}
	// A plain memory file next to them is still writable.
	plain := filepath.Join(root, "notes.md")
	if out := r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": plain, "content": "a fact"}}); strings.HasPrefix(out, "Error") {
		t.Fatalf("write to a plain memory refused: %q", out)
	}
}
