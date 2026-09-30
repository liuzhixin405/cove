package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// saveWithArchive saves session id, then compacts it so the rewrite leaves
// an archived transcript in archive/<id>/.
func saveWithArchive(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	r := &Record{ID: id, Model: "m", Messages: []api.Message{{Role: "user", Content: id + " one"}, {Role: "assistant", Content: "ok"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Messages = []api.Message{{Role: "user", Content: "[会话摘要] " + id}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if !archivedContaining(t, s, id, id+" one") {
		t.Fatalf("fixture: no archive for %s", id)
	}
	s.setIndexUpdatedAt(t, id, at)
}

// Prune removed <id>.jsonl and <id>.json but left archive/<id>/ behind
// (only Delete removed it), so up to five transcripts of every pruned
// session piled up for good.
func TestPruneRemovesArchiveOfPrunedSession(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		saveWithArchive(t, s, fmt.Sprintf("p%d", i), base.Add(time.Duration(i)*time.Hour))
	}
	removed, err := s.Prune(2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	if _, err := os.Stat(s.archiveDir("p0")); !os.IsNotExist(err) {
		t.Fatalf("archive of the pruned session left behind (stat err %v)", err)
	}
	for _, kept := range []string{"p1", "p2"} {
		if _, err := os.Stat(s.archiveDir(kept)); err != nil {
			t.Fatalf("archive of kept session %s removed: %v", kept, err)
		}
	}
}

// Archives whose session is gone (pruned before this fix, deleted by hand)
// are cleaned up by AutoPrune; those of existing and protected sessions stay.
func TestAutoPruneRemovesOrphanArchives(t *testing.T) {
	s := newTestStore(t)
	saveWithArchive(t, s, "live", time.Now())
	orphan := s.archiveDir("gone")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectedOrphan := s.archiveDir("in-use")
	if err := os.MkdirAll(protectedOrphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AutoPrune(10, "in-use"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan archive left behind (stat err %v)", err)
	}
	if _, err := os.Stat(s.archiveDir("live")); err != nil {
		t.Fatalf("archive of an existing session removed: %v", err)
	}
	if _, err := os.Stat(protectedOrphan); err != nil {
		t.Fatalf("archive of the protected session removed: %v", err)
	}
}

// Save took the size and mtime it records from a fresh stat after its own
// write: bytes another process appended in between were adopted as this
// process's, the next save appended after them without noticing, and a
// later rewrite (a metadata refresh) dropped them without archiving. The
// recorded size is now what this process wrote; a file that grew past it is
// another writer's, and the next save archives it first.
func TestSaveDoesNotAdoptBytesAppendedByAnotherWriter(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "raced", Model: "m", Messages: []api.Message{{Role: "user", Content: "start"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	foreign := `{"role":"user","content":"turn from another process"}` + "\n"
	orig := afterSessionWrite
	t.Cleanup(func() { afterSessionWrite = orig })
	afterSessionWrite = func(path string) {
		afterSessionWrite = orig // once
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(foreign)
		_ = f.Close()
	}
	r.Messages = append(r.Messages, api.Message{Role: "assistant", Content: "reply"})
	if err := s.Save(r); err != nil { // the other process appends right after our append
		t.Fatal(err)
	}
	r.Title = "renamed" // forces a rewrite that keeps this process's history
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.path("raced"))
	if !strings.Contains(string(data), "turn from another process") && !archivedContaining(t, s, "raced", "turn from another process") {
		t.Fatalf("the other writer's appended turn was dropped unarchived; file:\n%s", data)
	}
}

// The same race on a rewrite: the recorded size must be the rewritten
// length, not the stat of a file someone appended to right after.
func TestRewriteDoesNotAdoptBytesAppendedByAnotherWriter(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "raced2", Model: "m", Messages: []api.Message{{Role: "user", Content: "start"}}}
	foreign := `{"role":"user","content":"appended after our rewrite"}` + "\n"
	orig := afterSessionWrite
	t.Cleanup(func() { afterSessionWrite = orig })
	afterSessionWrite = func(path string) {
		afterSessionWrite = orig
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(foreign)
		_ = f.Close()
	}
	if err := s.Save(r); err != nil { // first save = rewrite
		t.Fatal(err)
	}
	r.Title = "renamed"
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.path("raced2"))
	if !strings.Contains(string(data), "appended after our rewrite") && !archivedContaining(t, s, "raced2", "appended after our rewrite") {
		t.Fatalf("the other writer's turn was dropped unarchived; file:\n%s", data)
	}
}
