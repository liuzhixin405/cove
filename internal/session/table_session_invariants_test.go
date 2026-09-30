package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

// Shared helpers of the T2 session-store tables (table_session_*_test.go).
//
// checkStoreInvariants is run after every table operation. It holds whatever
// the operation was, so a case author does not have to declare it:
//   - no atomic-write temp file (or *.tmp) is left anywhere under s.dir;
//   - every file in s.dir is <id>.jsonl, <id>.json, index.json or inside
//     archive/<key>/<unix-nanos>.jsonl;
//   - List() agrees with index.json after it ran (same IDs, every entry
//     names an existing file under its own key, with that file's size).

// outcome is the expected result class of one operation.
type outcome int

const (
	wantOK       outcome = iota
	wantNotExist         // errors.Is(err, fs.ErrNotExist)
	wantReserved         // errors.Is(err, ErrReservedID)
	wantRefused          // ErrNotExist or ErrReservedID: "no such session"
	wantErr              // any non-nil error
)

func (o outcome) String() string {
	switch o {
	case wantOK:
		return "ok"
	case wantNotExist:
		return "ErrNotExist"
	case wantReserved:
		return "ErrReservedID"
	case wantRefused:
		return "ErrNotExist|ErrReservedID"
	case wantErr:
		return "error"
	}
	return "?"
}

func (o outcome) match(err error) bool {
	switch o {
	case wantOK:
		return err == nil
	case wantNotExist:
		return errors.Is(err, fs.ErrNotExist)
	case wantReserved:
		return errors.Is(err, ErrReservedID)
	case wantRefused:
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrReservedID)
	case wantErr:
		return err != nil
	}
	return false
}

// tableRecord is the record the tables save: a real model name, so List
// does not hide it as a test session.
func tableRecord(id string, contents ...string) *Record {
	r := &Record{ID: id, Title: "t", Model: "claude-opus-5"}
	for _, c := range contents {
		r.Messages = append(r.Messages, api.Message{Role: "user", Content: c})
	}
	return r
}

// newTableStore returns a Store whose directory is <root>/sessions, so the
// tests can plant sentinels in <root>, outside the store.
//
// The tables never call NewStore (the only path to ~/.cove via
// getSessionDir), so unlike newTestStore they do not redirect HOME; that
// keeps t.Setenv out and lets every table test run in parallel, which is
// what keeps them within the package's time budget on Windows, where each
// file create costs milliseconds.
func newTableStore(t *testing.T) (s *Store, root string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return NewStoreAt(dir), root
}

func checkStoreInvariants(t *testing.T, s *Store, ctx string) {
	t.Helper()
	checkNoTempResidue(t, s.dir, ctx)
	checkDirLayout(t, s.dir, ctx)
	checkListMatchesIndex(t, s, ctx)
}

func checkNoTempResidue(t *testing.T, dir, ctx string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fsatomic.IsTempName(d.Name()) || strings.HasSuffix(d.Name(), ".tmp") {
			t.Errorf("%s: 临时文件残留 %q (got residue, want none)", ctx, p)
		}
		return nil
	})
}

var archiveName = regexp.MustCompile(`^[0-9]+\.jsonl$`)

func checkDirLayout(t *testing.T, dir, ctx string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("%s: ReadDir(%q): %v", ctx, dir, err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name != "archive" {
				t.Errorf("%s: s.dir 里有意外的目录 %q (want only archive/)", ctx, name)
			}
			continue
		}
		if fsatomic.IsTempName(name) {
			continue // reported by checkNoTempResidue
		}
		if name != indexFileName && !IsSessionFile(name) {
			t.Errorf("%s: s.dir 里有非会话文件 %q (want <id>.jsonl, <id>.json or index.json)", ctx, name)
		}
	}
	keys, _ := os.ReadDir(filepath.Join(dir, "archive"))
	for _, k := range keys {
		if !k.IsDir() {
			t.Errorf("%s: archive/ 里有文件 %q (want only archive/<key>/)", ctx, k.Name())
			continue
		}
		files, _ := os.ReadDir(filepath.Join(dir, "archive", k.Name()))
		for _, f := range files {
			if f.IsDir() || !archiveName.MatchString(f.Name()) {
				t.Errorf("%s: 归档目录里有意外条目 archive/%s/%s (want <unix-nanos>.jsonl)", ctx, k.Name(), f.Name())
			}
		}
	}
}

// checkListMatchesIndex runs List (which repairs the index) and compares it
// with index.json as List left it.
func checkListMatchesIndex(t *testing.T, s *Store, ctx string) {
	t.Helper()
	recs, err := s.List()
	if err != nil {
		t.Errorf("%s: List: got %v, want nil", ctx, err)
		return
	}
	idx, ierr := s.readIndex()
	if ierr != nil && !(errors.Is(ierr, fs.ErrNotExist) && len(recs) == 0) {
		t.Errorf("%s: index.json after List: got %v, want a readable index", ctx, ierr)
		return
	}
	var listed, indexed []string
	for _, r := range recs {
		listed = append(listed, r.ID)
	}
	for key, e := range idx.Sessions {
		if e.Model != "test-model" {
			indexed = append(indexed, e.ID)
		}
		if e.File != key+jsonlExt && e.File != key+legacyExt {
			t.Errorf("%s: index 条目 %q 的 file got %q, want %q or %q", ctx, key, e.File, key+jsonlExt, key+legacyExt)
		}
		info, err := os.Stat(filepath.Join(s.dir, e.File))
		if err != nil {
			t.Errorf("%s: index 条目 %q 指向不存在的文件 %q: %v", ctx, key, e.File, err)
			continue
		}
		if info.Size() != e.Size {
			t.Errorf("%s: index 条目 %q size got %d, file has %d", ctx, key, e.Size, info.Size())
		}
	}
	sort.Strings(listed)
	sort.Strings(indexed)
	if strings.Join(listed, "\x00") != strings.Join(indexed, "\x00") {
		t.Errorf("%s: List() 与 index.json 不一致: List got %q, index has %q", ctx, listed, indexed)
	}
}

// sentinels are files the operations must never touch: everything under
// root outside s.dir, a sibling session with its archive, and the sibling's
// index entry.
type sentinels struct {
	root      string
	outside   map[string][]byte // path relative to root -> bytes
	inside    map[string][]byte // path relative to s.dir -> bytes
	siblingIx []byte
}

const sentinelSibling = "sibling"

func plantSentinels(t *testing.T, s *Store, root string) *sentinels {
	t.Helper()
	sn := &sentinels{root: root, outside: map[string][]byte{}, inside: map[string][]byte{}}
	put := func(base, rel string, data []byte, into map[string][]byte) {
		p := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		into[filepath.FromSlash(rel)] = data
	}
	// In the parent of s.dir: what a traversal or a joined "archive/.."
	// would reach.
	put(root, "evil.jsonl", []byte("parent evil jsonl"), sn.outside)
	put(root, "evil.json", []byte("parent evil json"), sn.outside)
	put(root, "index.json", []byte("parent index"), sn.outside)
	put(root, "archive/evil/1.jsonl", []byte("parent archive"), sn.outside)

	// A sibling session with its index entry, written as Save would lay
	// them out (plantSession) but without the fsyncs, which dominate the
	// table's run time on Windows.
	plantSession(t, s.dir, tableRecord(sentinelSibling, "sibling turn"), time.Now())
	data, err := os.ReadFile(filepath.Join(s.dir, sentinelSibling+jsonlExt))
	if err != nil {
		t.Fatal(err)
	}
	sn.inside[sentinelSibling+jsonlExt] = data
	put(s.dir, "archive/"+sentinelSibling+"/100.jsonl", []byte("sibling archive"), sn.inside)
	idx, err := s.readIndex()
	if err != nil {
		t.Fatal(err)
	}
	sn.siblingIx, _ = json.Marshal(idx.Sessions[sentinelSibling])
	return sn
}

// plantSession writes r as <key>.jsonl (meta line + one line per message)
// with UpdatedAt = at, and adds its entry to index.json, matching what a Save
// by another process leaves behind.
func plantSession(t *testing.T, dir string, r *Record, at time.Time) {
	t.Helper()
	r.UpdatedAt = at
	r.CreatedAt = at
	var buf bytes.Buffer
	for _, v := range append([]any{metaOf(r)}, anySlice(r.Messages)...) {
		line, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	name := fileKey(r.ID) + jsonlExt
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStoreAt(dir)
	idx, _ := s.readIndex()
	idx.Version = indexVersion
	idx.Sessions[fileKey(r.ID)] = entryFor(r, name, info)
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(dir, indexFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func anySlice(ms []api.Message) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func (sn *sentinels) verify(t *testing.T, s *Store, ctx string) {
	t.Helper()
	// Outside s.dir: exactly the planted files, byte for byte.
	found := map[string]bool{}
	_ = filepath.WalkDir(sn.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == s.dir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(sn.root, p)
		found[rel] = true
		if _, ok := sn.outside[rel]; !ok {
			t.Errorf("%s: s.dir 之外出现新文件 %q (want none)", ctx, rel)
		}
		return nil
	})
	for rel, want := range sn.outside {
		if !found[rel] {
			t.Errorf("%s: s.dir 之外的哨兵 %q 被删除", ctx, rel)
			continue
		}
		if got, _ := os.ReadFile(filepath.Join(sn.root, rel)); !bytes.Equal(got, want) {
			t.Errorf("%s: s.dir 之外的哨兵 %q 被改写: got %q, want %q", ctx, rel, got, want)
		}
	}
	// The sibling session and its archive.
	for rel, want := range sn.inside {
		got, err := os.ReadFile(filepath.Join(s.dir, rel))
		if err != nil {
			t.Errorf("%s: 其他会话的文件 %q 不见了: %v", ctx, rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: 其他会话的文件 %q 被改写: got %q, want %q", ctx, rel, got, want)
		}
	}
	arch, _ := os.ReadDir(s.archiveDir(sentinelSibling))
	if len(arch) != 1 {
		t.Errorf("%s: 其他会话的归档 archive/%s 条目数 got %d, want 1", ctx, sentinelSibling, len(arch))
	}
	idx, err := s.readIndex()
	if err != nil {
		t.Errorf("%s: index.json 不可读: %v (其他会话的索引条目丢失)", ctx, err)
		return
	}
	got, _ := json.Marshal(idx.Sessions[sentinelSibling])
	if !bytes.Equal(got, sn.siblingIx) {
		t.Errorf("%s: 其他会话的索引条目被改动: got %s, want %s", ctx, got, sn.siblingIx)
	}
}

// caseInsensitiveFS reports whether dir's file system folds case.
func caseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(p) }()
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func isWindows() bool { return runtime.GOOS == "windows" }

// runKnownBugs: COVE_T2_RUN_BUGS=1 runs the cases marked `skip: "bug: ..."`
// instead of skipping them, to check a fix (they fail until it lands).
func runKnownBugs() bool { return os.Getenv("COVE_T2_RUN_BUGS") == "1" }

// skipBug skips with reason unless COVE_T2_RUN_BUGS=1.
func skipBug(t *testing.T, reason string) {
	t.Helper()
	if !runKnownBugs() {
		t.Skip(reason)
	}
}
