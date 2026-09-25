package session

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/fsatomic"
)

// newTestStore returns a Store rooted at a per-test temp directory.
//
// NewStore() resolves ~/.cove/sessions, which a test must never write to, so
// the dir field is injected directly. HOME/USERPROFILE are redirected as well
// so that any accidental call into getSessionDir() from the code under test
// still cannot reach the developer's real session history.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return &Store{dir: t.TempDir()}
}

// writeRawSession writes a session file byte-for-byte, bypassing Save. List()
// sorts on UpdatedAt and Save() stamps UpdatedAt with time.Now(), so tests that
// need controlled timestamps (or deliberately corrupt bytes) must write the
// file themselves.
func writeRawSession(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func recordJSON(t *testing.T, r Record) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(data)
}

func TestSaveLoadRoundTripsRecordFaithfully(t *testing.T) {
	s := newTestStore(t)

	created := time.Date(2026, 3, 4, 5, 6, 7, 890123000, time.UTC)
	want := &Record{
		ID:        "sess-round-trip",
		CreatedAt: created,
		Title:     "重构会话存储",
		Model:     "claude-opus-5",
		TokensIn:  1234,
		TokensOut: 5678,
		Cost:      0.4275,
		Messages: []api.Message{
			{Role: "user", Content: "请读取 main.go"},
			{
				Role:    "assistant",
				Content: "好的,我来读取。",
				ToolCalls: []api.ToolCall{{
					ID:   "call_1",
					Name: "read_file",
					// JSON numbers decode back as float64; using float64 here
					// keeps the DeepEqual below an equality check rather than a
					// type-tolerance check.
					Input: map[string]any{"path": "main.go", "limit": float64(10)},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Name: "read_file", Content: "package main"},
			{Role: "user", Content: "[system: continue]", Synthetic: true},
			{
				Role:  "user",
				Parts: []api.MessagePart{{Type: "image", MimeType: "image/png", Data: "aGk=", FileName: "shot.png"}},
			},
		},
	}

	beforeSave := time.Now()
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if want.UpdatedAt.Before(beforeSave) {
		t.Fatalf("Save did not stamp UpdatedAt: got %v, want >= %v", want.UpdatedAt, beforeSave)
	}

	got, err := s.Load("sess-round-trip")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
	if got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if got.TokensIn != want.TokensIn || got.TokensOut != want.TokensOut {
		t.Errorf("tokens = %d/%d, want %d/%d", got.TokensIn, got.TokensOut, want.TokensIn, want.TokensOut)
	}
	if got.Cost != want.Cost {
		t.Errorf("Cost = %v, want %v", got.Cost, want.Cost)
	}
	// time.Time carries a monotonic reading and a *time.Location that do not
	// survive JSON, so compare instants rather than struct values.
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
	if !reflect.DeepEqual(got.Messages, want.Messages) {
		t.Errorf("Messages mismatch:\n got %#v\nwant %#v", got.Messages, want.Messages)
	}
}

func TestSaveSurfacesMarshalErrorAndKeepsExistingFile(t *testing.T) {
	s := newTestStore(t)

	good := &Record{
		ID:       "sess-keepme",
		Title:    "good title",
		Model:    "claude-opus-5",
		Messages: []api.Message{{Role: "user", Content: "keep this conversation"}},
	}
	if err := s.Save(good); err != nil {
		t.Fatalf("Save(good): %v", err)
	}
	onDiskBefore, err := os.ReadFile(s.path("sess-keepme"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}

	// NaN is not representable in JSON, so MarshalIndent fails and returns no
	// bytes. Save must report that instead of writing the empty/partial result.
	broken := &Record{
		ID:    "sess-keepme",
		Title: "should never be written",
		Messages: []api.Message{{
			Role:      "assistant",
			ToolCalls: []api.ToolCall{{ID: "c1", Name: "bad", Input: map[string]any{"x": math.NaN()}}},
		}},
	}
	err = s.Save(broken)
	if err == nil {
		t.Fatal("Save returned nil for an unserializable record; the marshal error was swallowed")
	}
	if !strings.Contains(err.Error(), "marshal session") || !strings.Contains(err.Error(), "sess-keepme") {
		t.Errorf("error %q does not identify the failure or the session id", err)
	}

	onDiskAfter, err := os.ReadFile(s.path("sess-keepme"))
	if err != nil {
		t.Fatalf("existing session file disappeared after a failed Save: %v", err)
	}
	if string(onDiskAfter) != string(onDiskBefore) {
		t.Errorf("failed Save clobbered the existing session file:\n got %s\nwant %s", onDiskAfter, onDiskBefore)
	}

	// The good record must still be loadable and unchanged.
	reloaded, err := s.Load("sess-keepme")
	if err != nil {
		t.Fatalf("Load after failed Save: %v", err)
	}
	if reloaded.Title != "good title" {
		t.Errorf("Title = %q, want %q", reloaded.Title, "good title")
	}
}

func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "atomic", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "hi"}}}
	for i := 0; i < 3; i++ {
		if err := s.Save(r); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if fsatomic.IsTempName(e.Name()) {
			t.Errorf("Save left an atomic-write temp file behind: %s", e.Name())
		}
	}
	if len(names) != 2 || names[0] != "atomic.jsonl" || names[1] != indexFileName {
		t.Errorf("directory contains %v, want exactly [atomic.jsonl %s]", names, indexFileName)
	}
}

func TestSaveFailsWhenDirMissing(t *testing.T) {
	// Save does not create its directory (only NewStore does), so the atomic
	// write must fail loudly rather than silently dropping the session.
	s := &Store{dir: filepath.Join(t.TempDir(), "does-not-exist")}
	err := s.Save(&Record{ID: "x"})
	if err == nil {
		t.Fatal("Save into a missing directory returned nil error")
	}
	if !strings.Contains(err.Error(), "temp") {
		t.Errorf("error %q does not mention the failed atomic write", err)
	}
}

func TestSavePathStaysInsideStoreDir(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "../../evil", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "traversal"}}}
	if err := s.Save(r); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := os.Stat(filepath.Join(s.dir, "evil.jsonl")); err != nil {
		t.Fatalf("expected evil.jsonl inside the store dir: %v", err)
	}
	parent := filepath.Dir(filepath.Dir(s.dir))
	if _, err := os.Stat(filepath.Join(parent, "evil.jsonl")); err == nil {
		t.Fatalf("session id escaped the store directory into %s", parent)
	}

	got, err := s.Load("../../evil")
	if err != nil {
		t.Fatalf("Load with traversal id: %v", err)
	}
	if got.ID != "../../evil" {
		t.Errorf("ID = %q, want %q", got.ID, "../../evil")
	}
}

func TestListFiltersNonJSONAndTempFiles(t *testing.T) {
	s := newTestStore(t)

	realRec := Record{
		ID: "real", Title: "real session", Model: "claude-opus-5",
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Messages:  []api.Message{{Role: "user", Content: "hello"}},
	}
	writeRawSession(t, s.dir, "real.json", recordJSON(t, realRec))

	// A leftover in-progress atomic write: fsatomic names temp files
	// ".cove-tmp-<target>.<random>", so this is the exact shape a crash between
	// create and rename leaves behind.
	leftover := Record{ID: "leftover-temp", Title: "half written", Model: "claude-opus-5"}
	tempName := ".cove-tmp-real.json.1234567890"
	if !fsatomic.IsTempName(tempName) {
		t.Fatalf("fixture %q is not recognised as a temp name; the test is not exercising the real case", tempName)
	}
	writeRawSession(t, s.dir, tempName, recordJSON(t, leftover))

	// Non-session files that share the directory.
	writeRawSession(t, s.dir, "notes.md", "# not a session")
	writeRawSession(t, s.dir, "real.json.bak", recordJSON(t, Record{ID: "backup", Model: "claude-opus-5"}))
	if err := os.Mkdir(filepath.Join(s.dir, "nested.json"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	records, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 {
		var ids []string
		for _, r := range records {
			ids = append(ids, r.ID)
		}
		t.Fatalf("List returned %d records (%v), want only [real]", len(records), ids)
	}
	if records[0].ID != "real" {
		t.Errorf("List returned %q, want %q", records[0].ID, "real")
	}
}

func TestListToleratesCorruptFiles(t *testing.T) {
	s := newTestStore(t)

	good := Record{
		ID: "good", Title: "survivor", Model: "claude-opus-5",
		UpdatedAt: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC),
		Messages:  []api.Message{{Role: "user", Content: "still here"}},
	}
	writeRawSession(t, s.dir, "good.json", recordJSON(t, good))
	writeRawSession(t, s.dir, "truncated.json", `{"id":"truncated","messages":[{"role":"user"`)
	writeRawSession(t, s.dir, "empty.json", "")
	writeRawSession(t, s.dir, "garbage.json", "\x00\x01not json at all")
	writeRawSession(t, s.dir, "wrongtype.json", `["a","b"]`)

	records, err := s.List()
	if err != nil {
		t.Fatalf("List must not fail because of one corrupt file: %v", err)
	}
	if len(records) != 1 || records[0].ID != "good" {
		t.Fatalf("List = %+v, want just the good record", records)
	}
	if records[0].Title != "survivor" {
		t.Errorf("Title = %q, want %q", records[0].Title, "survivor")
	}
}

func TestListSortsByUpdatedAtDescending(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		id    string
		delta time.Duration
	}{
		{"oldest", 0},
		{"newest", 2 * time.Hour},
		{"middle", 1 * time.Hour},
	} {
		writeRawSession(t, s.dir, tc.id+".json", recordJSON(t, Record{
			ID: tc.id, Model: "claude-opus-5",
			CreatedAt: base, UpdatedAt: base.Add(tc.delta),
		}))
	}

	records, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"newest", "middle", "oldest"}
	if len(records) != len(want) {
		t.Fatalf("List returned %d records, want %d", len(records), len(want))
	}
	for i, id := range want {
		if records[i].ID != id {
			var got []string
			for _, r := range records {
				got = append(got, r.ID)
			}
			t.Fatalf("List order = %v, want %v", got, want)
		}
	}
}

func TestListSkipsTestModelSessions(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	writeRawSession(t, s.dir, "fixture.json", recordJSON(t, Record{
		ID: "fixture", Model: "test-model", UpdatedAt: base,
		Messages: []api.Message{{Role: "user", Content: "from a test run"}},
	}))
	writeRawSession(t, s.dir, "genuine.json", recordJSON(t, Record{
		ID: "genuine", Model: "claude-opus-5", UpdatedAt: base,
		Messages: []api.Message{{Role: "user", Content: "a real conversation"}},
	}))

	records, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].ID != "genuine" {
		t.Fatalf("List = %+v, want only the genuine session (model==\"test-model\" is filtered)", records)
	}
}

func TestListPopulatesPreviewAndTurnCounts(t *testing.T) {
	s := newTestStore(t)

	long := strings.Repeat("重构", 40) // 80 runes, well past the 50-rune preview cap
	writeRawSession(t, s.dir, "meta.json", recordJSON(t, Record{
		ID: "meta", Model: "claude-opus-5",
		UpdatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Messages: []api.Message{
			{Role: "user", Content: "[system: injected nudge]", Synthetic: true},
			{Role: "user", Content: "[Conversation Summary] earlier context"},
			{Role: "user", Content: "  " + long + "  "},
			{Role: "assistant", Content: "ok"},
			{Role: "tool", Content: "result"},
			{Role: "user", Content: "继续"},
		},
	}))

	records, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("List returned %d records, want 1", len(records))
	}
	got := records[0]
	if got.MessageCount != 6 {
		t.Errorf("MessageCount = %d, want 6", got.MessageCount)
	}
	if got.UserTurns != 2 {
		t.Errorf("UserTurns = %d, want 2 (synthetic and summary turns excluded)", got.UserTurns)
	}
	wantPreview := strings.Repeat("重构", 25) + "..."
	if got.Preview != wantPreview {
		t.Errorf("Preview = %q, want %q", got.Preview, wantPreview)
	}
	if !utf8.ValidString(got.Preview) {
		t.Errorf("Preview %q is not valid UTF-8", got.Preview)
	}
}

func TestListEmptyAndMissingDir(t *testing.T) {
	s := newTestStore(t)
	records, err := s.List()
	if err != nil {
		t.Fatalf("List on empty dir: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("List on empty dir returned %d records, want 0", len(records))
	}

	missing := &Store{dir: filepath.Join(t.TempDir(), "nope")}
	if _, err := missing.List(); err == nil {
		t.Error("List on a missing directory returned nil error")
	}
}

func TestLoadMissingFileReturnsError(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Load("not-there")
	if err == nil {
		t.Fatal("Load of a missing session returned nil error")
	}
	if !strings.Contains(err.Error(), "load session") || !strings.Contains(err.Error(), "not-there") {
		t.Errorf("error %q does not say which session failed to load", err)
	}
	if !os.IsNotExist(unwrapAll(err)) {
		t.Errorf("error %q does not wrap fs.ErrNotExist", err)
	}
}

func TestLoadMalformedFileReturnsParseError(t *testing.T) {
	s := newTestStore(t)
	writeRawSession(t, s.dir, "broken.json", `{"id":"broken","messages":[{"role":`)

	_, err := s.Load("broken")
	if err == nil {
		t.Fatal("Load of a truncated session returned nil error")
	}
	if !strings.Contains(err.Error(), "parse session") {
		t.Errorf("error %q does not identify a parse failure", err)
	}
}

// unwrapAll walks an error chain to its root so os.IsNotExist can be applied to
// a wrapped syscall error.
func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		next := u.Unwrap()
		if next == nil {
			return err
		}
		err = next
	}
}

func TestCompactPreviewClipsOnRuneBoundary(t *testing.T) {
	s := "更新配置文件并运行测试以确认修复生效" // 18 runes, 54 bytes
	for n := 0; n <= 20; n++ {
		got := compactPreview(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("compactPreview(_, %d) = %q, not valid UTF-8", n, got)
		}
		if n >= utf8.RuneCountInString(s) {
			if got != s {
				t.Fatalf("compactPreview(_, %d) = %q, want the unclipped input", n, got)
			}
			continue
		}
		wantRunes := []rune(s)[:n]
		if got != string(wantRunes)+"..." {
			t.Fatalf("compactPreview(_, %d) = %q, want %q", n, got, string(wantRunes)+"...")
		}
	}
}

func TestTrimWhitespaceLineCollapsesRuns(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  hello\n\tworld  ", "hello world"},
		{"a\r\n\r\nb", "a b"},
		{"   ", ""},
		{"", ""},
		{"读取\n\n文件", "读取 文件"},
		{"no-change", "no-change"},
	} {
		if got := trimWhitespaceLine(tc.in); got != tc.want {
			t.Errorf("trimWhitespaceLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstUserPreviewSkipsSyntheticContent(t *testing.T) {
	type msg = struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		Synthetic bool   `json:"synthetic,omitempty"`
	}
	messages := []msg{
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "   ", Synthetic: false},
		{Role: "user", Content: "[系统检测到重复操作循环] 换个方法"},
		{Role: "user", Content: "真正的请求"},
	}
	if got := firstUserPreview(messages); got != "真正的请求" {
		t.Errorf("firstUserPreview = %q, want %q", got, "真正的请求")
	}
	if got := firstUserPreview(nil); got != "" {
		t.Errorf("firstUserPreview(nil) = %q, want empty", got)
	}
}
