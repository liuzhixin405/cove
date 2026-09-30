package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// Dot files in a memory directory are bookkeeping (the dream consolidation
// lock, the memory write lock, the extraction record), never memories: All
// skips them. validName accepted them, so Save(".consolidate-lock") from a
// model-chosen name replaced the lock a running consolidation held, and
// Delete could remove it.
func TestDotNamesAreNotMemories(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreForDirs(dir)
	lock := filepath.Join(dir, ".consolidate-lock")
	if err := os.WriteFile(lock, []byte("4242"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".consolidate-lock", ".consolidate-lock.takeover", ".memory-write.lock", ".last-extraction.json", ".hidden.md", "."} {
		if err := s.Save(name, "done 1"); err == nil {
			t.Errorf("Save(%q) accepted", name)
		}
		if _, err := s.Append(name, "done 1"); err == nil {
			t.Errorf("Append(%q) accepted", name)
		}
		if err := s.Delete(name); err == nil {
			t.Errorf("Delete(%q) accepted", name)
		}
	}
	if data, err := os.ReadFile(lock); err != nil || string(data) != "4242" {
		t.Fatalf("lock = %q, %v; want untouched", data, err)
	}
	if err := s.Save("notes.md", "a fact"); err != nil {
		t.Fatalf("plain name refused: %v", err)
	}
}
