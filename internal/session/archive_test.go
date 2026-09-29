package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// A rewrite that drops messages (compaction) keeps the old transcript in the
// archive; appending and a metadata-only rewrite archive nothing.
func TestRewriteArchivesDroppedTranscript(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "c1", Messages: []api.Message{
		{Role: "user", Content: "original request"},
		{Role: "assistant", Content: "long answer"},
	}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Messages = append(r.Messages, api.Message{Role: "user", Content: "more"})
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.archiveDir("c1")); !os.IsNotExist(err) {
		t.Fatalf("an append archived the session (%v)", err)
	}

	r.Messages = []api.Message{{Role: "user", Content: "<original_request>original request</original_request> summary"}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(s.archiveDir("c1"), "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("archive holds %d files, want 1", len(files))
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "long answer") || !strings.Contains(string(data), "more") {
		t.Fatalf("archive lacks the dropped messages:\n%s", data)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d sessions (%v), want 1: the archive must not appear as a session", len(list), err)
	}

	if err := s.Delete("c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.archiveDir("c1")); !os.IsNotExist(err) {
		t.Fatal("Delete left the archive behind")
	}
}

func TestArchiveKeepsTheLatestFew(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "c2"}
	for i := 0; i < archiveMaxPerSession+3; i++ {
		r.Messages = []api.Message{{Role: "user", Content: "v" + itoaTest(i)}, {Role: "assistant", Content: "a"}}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(s.archiveDir("c2"), "*.jsonl"))
	if len(files) != archiveMaxPerSession {
		t.Fatalf("archive holds %d files, want %d", len(files), archiveMaxPerSession)
	}
}

func itoaTest(i int) string { return string(rune('a' + i)) }
