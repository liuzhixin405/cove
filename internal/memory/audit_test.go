package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dream consolidation lock (.consolidate-lock) lives in the memory
// directory and holds only a PID. Loading it as a memory put a bare process
// number into every system prompt as if it were something to remember.
func TestAllSkipsHiddenFiles(t *testing.T) {
	s := newTestStore(t, map[string]string{
		".consolidate-lock": "12345",
		"note.md":           "Use pnpm, not npm.",
	})
	for _, e := range s.All() {
		if strings.HasPrefix(e.Name, ".") {
			t.Fatalf("hidden file %q was loaded as a memory", e.Name)
		}
	}
	if prompt := s.BuildPrompt(); strings.Contains(prompt, "12345") {
		t.Fatalf("lock file content reached the prompt:\n%s", prompt)
	}
}

// A name is a file inside the memory directory; one with path elements must
// not write (or delete) a file somewhere else.
func TestSaveRejectsNamesThatLeaveTheMemoryDir(t *testing.T) {
	s := newTestStore(t, nil)
	outside := filepath.Join(filepath.Dir(s.dirs[0]), "escaped.md")
	for _, name := range []string{"../escaped.md", `..\escaped.md`, "sub/x.md", "", ".", ".."} {
		if err := s.Save(name, "content"); err == nil {
			t.Errorf("Save(%q) accepted a name that is not a plain file name", name)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("Save wrote outside the memory directory (stat err: %v)", err)
	}
}

func TestDeleteRejectsNamesThatLeaveTheMemoryDir(t *testing.T) {
	s := newTestStore(t, nil)
	victim := filepath.Join(filepath.Dir(s.dirs[0]), "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(victim) })

	if err := s.Delete("../victim.txt"); err == nil {
		t.Error("Delete accepted a name with a parent-directory element")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("Delete removed a file outside the memory directory: %v", err)
	}
}

// /memory remove printed "已删除" for a name that never existed, because Delete
// returned nil when it found nothing.
func TestDeleteReportsMissingEntry(t *testing.T) {
	s := newTestStore(t, map[string]string{"real.md": "x"})
	if err := s.Delete("typo.md"); err == nil {
		t.Fatal("Delete of a nonexistent memory reported success")
	}
	if err := s.Delete("real.md"); err != nil {
		t.Fatalf("Delete of an existing memory failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dirs[0], "real.md")); !os.IsNotExist(err) {
		t.Fatalf("real.md still exists after Delete (stat err: %v)", err)
	}
}
