package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// archivedContaining reports whether any archived transcript of key holds want.
func archivedContaining(t *testing.T, s *Store, key, want string) bool {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(s.archiveDir(key), "*.jsonl"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil && strings.Contains(string(data), want) {
			return true
		}
	}
	return false
}

// Two cove processes on one session: each one's rewrite used to erase the
// turns the other had appended, since the file no longer matched this
// process's record (so it rewrote) and archiveIfDropping compared only
// against this process's record (so it archived nothing). The on-disk
// version is now archived before a rewrite replaces a file someone else
// changed.
func TestRewriteArchivesFileChangedByAnotherProcess(t *testing.T) {
	a := newTestStore(t)
	b := &Store{dir: a.dir} // a second process on the same directory

	base := []api.Message{{Role: "user", Content: "start"}, {Role: "assistant", Content: "ok"}}
	ra := &Record{ID: "shared", Messages: append([]api.Message(nil), base...)}
	if err := a.Save(ra); err != nil {
		t.Fatal(err)
	}
	rb, err := b.Load("shared")
	if err != nil {
		t.Fatal(err)
	}

	// Process B appends its own turn.
	rb.Messages = append(rb.Messages, api.Message{Role: "user", Content: "turn from process B"})
	if err := b.Save(rb); err != nil {
		t.Fatal(err)
	}
	// Process A, unaware, appends its turn: the file size no longer matches
	// its record, so it rewrites the file with only its own history.
	ra.Messages = append(ra.Messages, api.Message{Role: "user", Content: "turn from process A"})
	if err := a.Save(ra); err != nil {
		t.Fatal(err)
	}

	if !archivedContaining(t, a, "shared", "turn from process B") {
		t.Fatal("process B's appended turn was erased by process A's rewrite and not archived")
	}
	data, _ := os.ReadFile(a.path("shared"))
	if !strings.Contains(string(data), "turn from process A") {
		t.Fatalf("live file lacks process A's turn:\n%s", data)
	}
}

// A same-size change by someone else (the mtime differs) must not be taken
// for this process's own file and appended after, nor rewritten unarchived.
func TestSaveDetectsSameSizeExternalChange(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "same", Messages: []api.Message{{Role: "user", Content: "aaaa"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	path := s.path("same")
	data, _ := os.ReadFile(path)
	other := strings.Replace(string(data), "aaaa", "bbbb", 1)
	if err := os.WriteFile(path, []byte(other), 0600); err != nil {
		t.Fatal(err)
	}
	st := s.persisted[fileKey("same")]
	info, _ := os.Stat(path)
	if info.Size() != st.size {
		t.Fatalf("fixture changed the size (%d vs %d)", info.Size(), st.size)
	}
	// Make sure the mtime differs even on a coarse-grained file system.
	past := info.ModTime().Add(-10e9)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}

	r.Messages = append(r.Messages, api.Message{Role: "assistant", Content: "reply"})
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if !archivedContaining(t, s, "same", "bbbb") {
		t.Fatal("the externally changed file was overwritten without being archived")
	}
}
