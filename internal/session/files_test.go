package session

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

func TestIsSessionFile(t *testing.T) {
	for name, want := range map[string]bool{
		"a.jsonl":                    true,
		"b.json":                     true,
		"index.json":                 false,
		"index.jsonl":                false, // "index" is reserved for the index file
		".cove-tmp-a.jsonl.123":      false,
		"a.json.bak.20260925-120000": false,
		"notes.md":                   false,
		".jsonl":                     false,
		"":                           false,
	} {
		if got := IsSessionFile(name); got != want {
			t.Errorf("IsSessionFile(%q) = %v, want %v", name, got, want)
		}
	}
}

// a.jsonl, b.json and index.json: two sessions, and a legacy file that was
// already migrated is not counted twice.
func TestListSessionFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.jsonl", "b.json", "index.json", "c.jsonl", "c.json", "x.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "d.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ListSessionFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.jsonl", "b.json", "c.jsonl"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListSessionFiles = %v, want %v", got, want)
	}
	if SessionIDFromFile("a.jsonl") != "a" || SessionIDFromFile("b.json") != "b" {
		t.Error("SessionIDFromFile did not strip the extension")
	}
	if _, err := ListSessionFiles(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing directory must be reported")
	}
}

// Replace rewrites a session in full without restamping UpdatedAt, so a
// maintenance pass (/history clean) does not reorder the history list.
func TestReplaceKeepsUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreAt(dir)
	if err := s.Save(&Record{ID: "r", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Load("r")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rec.UpdatedAt = old
	rec.Title = "fixed"
	rec.Messages[0].Synthetic = true
	if err := s.Replace(rec); err != nil {
		t.Fatal(err)
	}
	got, err := NewStoreAt(dir).Load("r")
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(old) || got.Title != "fixed" || !got.Messages[0].Synthetic {
		t.Fatalf("after Replace: %+v", got)
	}
	list, _ := NewStoreAt(dir).List()
	if len(list) != 1 || !list[0].UpdatedAt.Equal(old) {
		t.Fatalf("List after Replace = %+v", list)
	}
}

// "index" names the sessions index, not a session: resuming it used to parse
// index.json as a legacy session and start a bogus conversation.
func TestIndexIsAReservedSessionID(t *testing.T) {
	for _, name := range []string{"index", "index.json", "index.jsonl", " index.json "} {
		if _, err := ParseSessionID(name); !errors.Is(err, ErrReservedID) {
			t.Errorf("ParseSessionID(%q) err = %v, want ErrReservedID", name, err)
		}
	}
	if id, err := ParseSessionID("s1.jsonl"); err != nil || id != "s1" {
		t.Errorf("ParseSessionID(s1.jsonl) = %q, %v", id, err)
	}
	if id, err := ParseSessionID("index-2"); err != nil || id != "index-2" {
		t.Errorf("ParseSessionID(index-2) = %q, %v", id, err)
	}

	dir := t.TempDir()
	store := NewStoreAt(dir)
	if err := store.Save(&Record{ID: "s1", Messages: []api.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Fatalf("index.json not written: %v", err)
	}
	if _, err := store.Load("index"); !errors.Is(err, ErrReservedID) {
		t.Fatalf("Load(index) err = %v, want ErrReservedID", err)
	}
}
