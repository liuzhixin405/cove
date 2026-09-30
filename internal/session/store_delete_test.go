package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// Delete removes the session file and its index entry; List no longer
// returns it and the neighbours are untouched.
func TestDeleteRemovesSessionAndIndexEntry(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"keep", "gone"} {
		r := &Record{ID: id, Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: id}}}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete("gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "gone.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("gone.jsonl still on disk: %v", err)
	}
	records, err := (&Store{dir: s.dir}).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != "keep" {
		t.Fatalf("List after Delete = %+v, want only keep", records)
	}
	idx, _ := s.readIndex()
	if _, ok := idx.Sessions["gone"]; ok {
		t.Fatalf("index.json still lists gone")
	}
	if _, err := s.Load("gone"); err == nil {
		t.Fatalf("Load(gone) succeeded after Delete")
	}
}

// A legacy .json session is deleted the same way.
func TestDeleteRemovesLegacyFile(t *testing.T) {
	s := newTestStore(t)
	writeRawSession(t, s.dir, "old.json", recordJSON(t, Record{
		ID: "old", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "old"}},
	}))
	if err := s.Delete("old"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "old.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old.json still on disk: %v", err)
	}
}

// Deleting a session that does not exist is an error, not a silent no-op:
// the caller reports counts to the user.
func TestDeleteUnknownSessionIsAnError(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Delete(nope) = %v, want fs.ErrNotExist", err)
	}
}

// A session ID that is not a plain name is "no such session", never a path:
// Delete("..") used to RemoveAll the store directory itself (archiveDir
// joined "archive/.." back onto it), and Delete("/") the whole archive.
func TestDeleteRefusesPathLikeIDs(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "keep", Model: "m", Messages: []api.Message{{Role: "user", Content: "hi"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.archiveDir("keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"..", "/", `\`, "../..", "archive/..", "", "  ", "."} {
		if err := s.Delete(id); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Delete(%q) = %v, want ErrNotExist", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "keep.jsonl")); err != nil {
		t.Fatalf("keep.jsonl gone: %v", err)
	}
	if _, err := os.Stat(s.archiveDir("keep")); err != nil {
		t.Fatalf("archive gone: %v", err)
	}
	// A hostile ID that still names a file is kept inside the store (Base),
	// as documented; only one that names no file is refused.
	if _, err := s.Load("../keep"); err != nil {
		t.Fatalf("Load(../keep) = %v, want it mapped to keep inside the store", err)
	}
	if _, err := s.Load(".."); err == nil {
		t.Fatal("Load(..) succeeded")
	}
}
