package session

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return bytes.Count(data, []byte("\n"))
}

// Appending 100 messages one Save at a time must leave one metadata line plus
// one line per message, and never rewrite what is already on disk.
func TestSaveAppendsOneLinePerMessage(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "append", Title: "追加", Model: "claude-opus-5", CreatedAt: time.Now()}
	if err := s.Save(r); err != nil {
		t.Fatalf("Save(empty): %v", err)
	}
	for i := 0; i < 100; i++ {
		r.Messages = append(r.Messages, api.Message{Role: "user", Content: fmt.Sprintf("消息 %d", i)})
		if err := s.Save(r); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}
	path := filepath.Join(s.dir, "append.jsonl")
	if got := countLines(t, path); got != 101 {
		t.Fatalf("append.jsonl has %d lines, want 101 (1 metadata + 100 messages)", got)
	}

	// A second store (a new process) sees every message.
	fresh := &Store{dir: s.dir}
	got, err := fresh.Load("append")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Messages) != 100 || got.Messages[99].Content != "消息 99" {
		t.Fatalf("Load returned %d messages (last %q), want 100", len(got.Messages), lastContent(got.Messages))
	}
	if got.Title != "追加" || got.Model != "claude-opus-5" {
		t.Errorf("metadata = %q/%q, want 追加/claude-opus-5", got.Title, got.Model)
	}
}

// The prefix already on disk must not be rewritten on an append: the first
// message line's bytes stay exactly as they were. (A token/cost change alone
// is deferred to the periodic first-line refresh; a title change refreshes
// the line on the next save — see TestTitleChangeRefreshesFirstLine.)
func TestSaveAppendDoesNotRewritePrefix(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "prefix", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "first"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "prefix.jsonl")
	before, _ := os.ReadFile(path)
	infoBefore, _ := os.Stat(path)

	r.Messages = append(r.Messages, api.Message{Role: "assistant", Content: "second"})
	r.TokensIn = 42
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, before) {
		t.Fatalf("append rewrote the existing content:\nbefore %q\nafter  %q", before, after)
	}
	infoAfter, _ := os.Stat(path)
	if !os.SameFile(infoBefore, infoAfter) {
		t.Errorf("append replaced the file instead of appending to it")
	}

	got, err := (&Store{dir: s.dir}).Load("prefix")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensIn != 42 {
		t.Errorf("updated metadata lost: tokens %d", got.TokensIn)
	}
}

// Compaction replaces the history; the store must notice and rewrite rather
// than append the new list after the old one.
func TestSaveRewritesWhenHistoryIsReplaced(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "compact", Model: "claude-opus-5"}
	for i := 0; i < 5; i++ {
		r.Messages = append(r.Messages, api.Message{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Messages = []api.Message{{Role: "user", Content: "[会话摘要] summary"}, {Role: "user", Content: "m4"}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	// Same length, different content.
	r.Messages = []api.Message{{Role: "user", Content: "other"}, {Role: "user", Content: "m4"}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	got, err := (&Store{dir: s.dir}).Load("compact")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "other" {
		t.Fatalf("after replacing history Load = %+v, want [other m4]", got.Messages)
	}
	if n := countLines(t, filepath.Join(s.dir, "compact.jsonl")); n != 3 {
		t.Errorf("compact.jsonl has %d lines, want 3", n)
	}
}

// List must answer from the index without decoding message bodies.
func TestListDoesNotDecodeMessageBodies(t *testing.T) {
	s := newTestStore(t)
	huge := strings.Repeat("x", 1<<20)
	for i := 0; i < 20; i++ {
		r := &Record{ID: fmt.Sprintf("big-%02d", i), Title: "big", Model: "claude-opus-5", Cwd: "/proj",
			Messages: []api.Message{{Role: "user", Content: "请看这个文件"}, {Role: "tool", Content: huge}}}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	// Warm up (the first List may build nothing, but must not be timed with
	// directory creation noise).
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	before := fileDecodes.Load()
	start := time.Now()
	records, err := (&Store{dir: s.dir}).List()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 20 {
		t.Fatalf("List returned %d records, want 20", len(records))
	}
	// The deterministic check: no session file was decoded.
	if n := fileDecodes.Load() - before; n != 0 {
		t.Errorf("List decoded %d session files, want 0 (the index must answer it)", n)
	}
	// A generous wall-clock bound as a backstop; decoding 20 MB would blow it.
	if elapsed > 500*time.Millisecond {
		t.Errorf("List took %v over 20 MB of sessions, want < 500ms", elapsed)
	}
	r := records[0]
	if r.MessageCount != 2 || r.UserTurns != 1 || r.Preview != "请看这个文件" || r.Cwd != "/proj" {
		t.Errorf("index metadata = count %d turns %d preview %q cwd %q", r.MessageCount, r.UserTurns, r.Preview, r.Cwd)
	}
	if r.Messages != nil {
		t.Errorf("List must not return message bodies")
	}
}

// A session in the old whole-file JSON format loads, and the next Save
// migrates it to JSONL and removes the old file.
func TestLegacyJSONSessionLoadsAndMigratesOnSave(t *testing.T) {
	s := newTestStore(t)
	legacy := Record{
		ID: "old", Title: "旧会话", Model: "claude-opus-5", Cwd: "/p",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Messages:  []api.Message{{Role: "user", Content: "你好"}, {Role: "assistant", Content: "hi"}},
	}
	writeRawSession(t, s.dir, "old.json", recordJSON(t, legacy))

	records, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != "old" || records[0].Preview != "你好" {
		t.Fatalf("List of a legacy session = %+v", records)
	}

	got, err := s.Load("old")
	if err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if got.Title != "旧会话" || len(got.Messages) != 2 {
		t.Fatalf("legacy Load = %+v", got)
	}
	got.Messages = append(got.Messages, api.Message{Role: "user", Content: "继续"})
	if err := s.Save(got); err != nil {
		t.Fatalf("Save migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "old.json")); !os.IsNotExist(err) {
		t.Errorf("old.json still present after migration (err=%v)", err)
	}
	if n := countLines(t, filepath.Join(s.dir, "old.jsonl")); n != 4 {
		t.Errorf("old.jsonl has %d lines, want 4", n)
	}
	again, err := (&Store{dir: s.dir}).Load("old")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 3 || !again.CreatedAt.Equal(legacy.CreatedAt) {
		t.Errorf("migrated session = %d messages, created %v", len(again.Messages), again.CreatedAt)
	}
	records, _ = s.List()
	if len(records) != 1 {
		t.Errorf("List after migration returned %d records, want 1", len(records))
	}
}

// A crash between appending to the JSONL file and updating the index leaves
// the index stale; List must notice (size mismatch) and rescan the file.
func TestListRescansFileWhenIndexIsStale(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "stale", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "one"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "stale.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"user","content":"two"}` + "\n")
	_ = f.Close()

	records, err := (&Store{dir: s.dir}).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].MessageCount != 2 || records[0].UserTurns != 2 {
		t.Fatalf("List after an unindexed append = %+v, want 2 messages", records)
	}
}

// Losing index.json entirely must not lose any session.
func TestListRebuildsMissingIndex(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"a", "b"} {
		if err := s.Save(&Record{ID: id, Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: id}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(s.dir, indexFileName)); err != nil {
		t.Fatal(err)
	}
	records, err := (&Store{dir: s.dir}).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("List without an index returned %d records, want 2", len(records))
	}
}

// A torn final line (crash mid-append) must not make the session unloadable,
// and the next Save must not append after the garbage.
func TestLoadToleratesTornLastLine(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "torn", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "kept"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(s.dir, "torn.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(`{"role":"assistant","content":"half`)
	_ = f.Close()

	fresh := &Store{dir: s.dir}
	got, err := fresh.Load("torn")
	if err != nil {
		t.Fatalf("Load with a torn tail: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "kept" {
		t.Fatalf("Load = %+v, want just the intact message", got.Messages)
	}
	got.Messages = append(got.Messages, api.Message{Role: "assistant", Content: "whole"})
	if err := fresh.Save(got); err != nil {
		t.Fatal(err)
	}
	again, err := (&Store{dir: s.dir}).Load("torn")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 2 || again.Messages[1].Content != "whole" {
		t.Fatalf("after Save the session = %+v", again.Messages)
	}
	data, _ := os.ReadFile(filepath.Join(s.dir, "torn.jsonl"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if strings.Contains(sc.Text(), "half") {
			t.Fatalf("the torn line survived the next Save: %q", data)
		}
	}
}

// Prune keeps the newest sessions and deletes the rest, legacy files included.
func TestPruneKeepsNewest(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 55; i++ {
		id := fmt.Sprintf("s%02d", i)
		r := &Record{ID: id, Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: id}}}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
		// Save stamps time.Now(); force a deterministic order via the index.
		s.setIndexUpdatedAt(t, id, base.Add(time.Duration(i)*time.Hour))
	}
	writeRawSession(t, s.dir, "legacy.json", recordJSON(t, Record{
		ID: "legacy", Model: "claude-opus-5", UpdatedAt: base.Add(-time.Hour),
		Messages: []api.Message{{Role: "user", Content: "old"}},
	}))

	removed, err := s.Prune(50)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 6 {
		t.Errorf("Prune removed %d, want 6", removed)
	}
	records, _ := (&Store{dir: s.dir}).List()
	if len(records) != 50 {
		t.Fatalf("after Prune(50) List has %d records", len(records))
	}
	for _, gone := range []string{"s00.jsonl", "s04.jsonl", "legacy.json"} {
		if _, err := os.Stat(filepath.Join(s.dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived Prune", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "s05.jsonl")); err != nil {
		t.Errorf("s05.jsonl (among the newest 50) was pruned: %v", err)
	}
	if n, err := s.Prune(0); err != nil || n != 0 {
		t.Errorf("Prune(0) = %d, %v; want a no-op", n, err)
	}
}

// setIndexUpdatedAt rewrites one index entry's UpdatedAt, leaving the file
// fingerprint alone so List keeps trusting the entry.
func (s *Store) setIndexUpdatedAt(t *testing.T, key string, ts time.Time) {
	t.Helper()
	idx, err := s.readIndex()
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}
	e := idx.Sessions[key]
	if e == nil {
		t.Fatalf("no index entry for %s", key)
	}
	e.UpdatedAt = ts
	if err := s.writeIndex(idx); err != nil {
		t.Fatalf("writeIndex: %v", err)
	}
}

func lastContent(ms []api.Message) string {
	if len(ms) == 0 {
		return ""
	}
	return ms[len(ms)-1].Content
}

// index.json is shared by every cove process, and on Windows replacing it can
// fail while another process has it open. The messages are already on disk by
// then, so a failed index update must not make Save report failure.
func TestSaveSucceedsWhenIndexWriteFails(t *testing.T) {
	s := newTestStore(t)
	old := writeIndexFile
	writeIndexFile = func(string, []byte) error { return fmt.Errorf("sharing violation (test)") }
	t.Cleanup(func() { writeIndexFile = old })

	r := &Record{ID: "busy", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "one"}}}
	if err := s.Save(r); err != nil {
		t.Fatalf("Save with a failing index write = %v, want nil", err)
	}
	r.Messages = append(r.Messages, api.Message{Role: "assistant", Content: "two"})
	if err := s.Save(r); err != nil {
		t.Fatalf("second Save = %v, want nil", err)
	}
	if n := countLines(t, filepath.Join(s.dir, "busy.jsonl")); n != 3 {
		t.Fatalf("busy.jsonl has %d lines, want 3: the messages must be on disk", n)
	}

	writeIndexFile = old
	records, err := (&Store{dir: s.dir}).List()
	if err != nil || len(records) != 1 || records[0].MessageCount != 2 {
		t.Fatalf("List after the index failures = %+v, %v; want the session rebuilt from its file", records, err)
	}
}

// Prune never deletes a protected (active) session, and protected sessions
// do not count against keep.
func TestPruneSkipsProtectedSessions(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("p%d", i)
		if err := s.Save(&Record{ID: id, Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: id}}}); err != nil {
			t.Fatal(err)
		}
		s.setIndexUpdatedAt(t, id, base.Add(time.Duration(i)*time.Hour))
	}
	removed, err := s.Prune(2, "p0")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("Prune removed %d, want 2", removed)
	}
	for id, want := range map[string]bool{"p0": true, "p1": false, "p2": false, "p3": true, "p4": true} {
		_, err := os.Stat(filepath.Join(s.dir, id+".jsonl"))
		if exists := err == nil; exists != want {
			t.Errorf("%s exists=%v, want %v", id, exists, want)
		}
	}
}

// Precondition pin (documents current behavior, not a wish): the append
// fingerprint (msgPrint) covers role, content, reasoning, IDs, counts and the
// first tool call's ID, but not ToolCall.Input or other nested fields. A
// message already on disk that is modified in place in one of those fields is
// therefore NOT noticed: Save appends only the new messages and the file keeps
// the old value. The engine never edits a persisted message's nested fields in
// place (compaction and /clear replace the history, which is detected); if
// that ever changes, this test fails and the fingerprint must grow.
func TestFingerprintMissesInPlaceNestedEdit(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "fp", Model: "claude-opus-5", Messages: []api.Message{
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "ls"}}}},
	}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Messages[0].ToolCalls[0].Input["command"] = "rm -rf build"
	r.Messages = append(r.Messages, api.Message{Role: "tool", ToolCallID: "c1", Content: "ok"})
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	got, err := (&Store{dir: s.dir}).Load("fp")
	if err != nil || len(got.Messages) != 2 {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if cmd := got.Messages[0].ToolCalls[0].Input["command"]; cmd != "ls" {
		t.Fatalf("persisted command = %v; the fingerprint now notices nested edits — update this pin", cmd)
	}
}
