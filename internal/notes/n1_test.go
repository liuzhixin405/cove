package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Notes an older version kept in <project>/.cove are moved to the data
// directory, and the emptied .cove directory is removed.
func TestNewMigratesLegacyNotes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	project := t.TempDir()
	legacyDir := filepath.Join(project, ".cove")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, "session_notes.md")
	if err := os.WriteFile(legacy, []byte("## Decisions\n\n- [10:00] legacy decision\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(project)
	s.Load()
	if len(s.entries) != 1 || s.entries[0].Text != "legacy decision" {
		t.Fatalf("entries after migration = %+v", s.entries)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file still there (err %v)", err)
	}
	if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
		t.Fatalf("empty legacy .cove dir still there (err %v)", err)
	}

	// A .cove directory holding anything else stays.
	project2 := t.TempDir()
	dir2 := filepath.Join(project2, ".cove")
	_ = os.MkdirAll(dir2, 0o700)
	_ = os.WriteFile(filepath.Join(dir2, "session_notes.md"), []byte("- [10:00] x\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir2, "other.json"), []byte("{}"), 0o644)
	New(project2)
	if _, err := os.Stat(filepath.Join(dir2, "other.json")); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "session_notes.md")); !os.IsNotExist(err) {
		t.Fatalf("legacy notes not moved (err %v)", err)
	}
}

// The same note is kept once.
func TestAddDeduplicatesByContent(t *testing.T) {
	s, _ := newTestNotes(t)
	s.AddDecision("用 tabs 缩进")
	s.AddDecision("用 tabs 缩进")
	s.AddDecision("  用 tabs 缩进 ")
	s.AddDiscovery("用 tabs 缩进") // another category is another note
	if len(s.entries) != 2 {
		t.Fatalf("entries = %+v, want 2", s.entries)
	}
	// Loading the file does not duplicate what is already in memory.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Load()
	if len(s.entries) != 2 {
		t.Fatalf("after Load: %d entries, want 2", len(s.entries))
	}
}

// Loaded notes keep their own time: every loaded note used to be stamped
// with the load time.
func TestLoadKeepsTimestamps(t *testing.T) {
	s, projectDir := newTestNotes(t)
	s.mu.Lock()
	s.entries = append(s.entries, NoteEntry{Timestamp: time.Date(2026, 3, 4, 5, 6, 0, 0, time.Local), Category: "decision", Text: "old one"})
	s.modified = true
	s.mu.Unlock()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	r := New(projectDir)
	r.Load()
	if len(r.entries) != 1 {
		t.Fatalf("loaded %d entries", len(r.entries))
	}
	if got := r.entries[0].Timestamp.Format("2006-01-02 15:04"); got != "2026-03-04 05:06" {
		t.Fatalf("timestamp = %s, want 2026-03-04 05:06", got)
	}

	// The old "- [15:04] text" lines keep their hour and minute.
	if err := os.WriteFile(r.path, []byte("## Decisions\n\n- [09:30] legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := New(projectDir)
	r2.Load()
	if len(r2.entries) != 1 || r2.entries[0].Timestamp.Format("15:04") != "09:30" {
		t.Fatalf("legacy entry = %+v", r2.entries)
	}
	if !strings.Contains(r2.entries[0].Text, "legacy") {
		t.Fatalf("text = %q", r2.entries[0].Text)
	}
}
