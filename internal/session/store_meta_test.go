package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

// firstLineMeta decodes the .jsonl first line — the fallback metadata used
// when index.json is lost.
func firstLineMeta(t *testing.T, path string) fileMeta {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	if !sc.Scan() {
		t.Fatalf("%s is empty", path)
	}
	var m fileMeta
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		t.Fatalf("first line: %v", err)
	}
	return m
}

// The first line used to be rewritten only by a full rewrite, so a title set
// after the first save stayed empty there for the life of the session: with
// index.json lost, /resume listed it untitled. A title change now rides along
// with the next save.
func TestTitleChangeRefreshesFirstLine(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "t", Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: "one"}}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Messages = append(r.Messages, api.Message{Role: "assistant", Content: "two"})
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "t.jsonl")
	if m := firstLineMeta(t, path); m.Title != "" {
		t.Fatalf("title %q before it was set", m.Title)
	}
	r.Title = "修复登录"
	r.Messages = append(r.Messages, api.Message{Role: "user", Content: "three"})
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if m := firstLineMeta(t, path); m.Title != "修复登录" {
		t.Fatalf("first-line title = %q after the title changed", m.Title)
	}
	if got := countLines(t, path); got != 4 {
		t.Fatalf("%d lines, want 1 metadata + 3 messages", got)
	}
	got, err := (&Store{dir: s.dir}).Load("t")
	if err != nil || len(got.Messages) != 3 {
		t.Fatalf("Load = %d messages, %v", len(got.Messages), err)
	}
	// Appending continues after the refresh: the next save is an append.
	r.Messages = append(r.Messages, api.Message{Role: "assistant", Content: "four"})
	if st := s.appendableState("t", path, r.Messages); st == nil {
		t.Fatal("the refresh left the session in rewrite-every-save state")
	}
}

// Tokens and cost change on every turn; rewriting the whole file for each
// would undo the append-only format, so they are refreshed at most every
// metaRefreshEvery appends (index.json always has the current values).
func TestCostChangeRefreshesFirstLinePeriodically(t *testing.T) {
	s := newTestStore(t)
	r := &Record{ID: "c", Title: "t", Model: "claude-opus-5"}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "c.jsonl")
	for i := 1; i < metaRefreshEvery; i++ {
		r.Cost = float64(i)
		r.Messages = append(r.Messages, api.Message{Role: "user", Content: fmt.Sprint(i)})
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	if m := firstLineMeta(t, path); m.Cost != 0 {
		t.Fatalf("cost refreshed after %d appends (cost %v); want it deferred", metaRefreshEvery-1, m.Cost)
	}
	r.Cost = 99
	r.Messages = append(r.Messages, api.Message{Role: "user", Content: "last"})
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if m := firstLineMeta(t, path); m.Cost != 99 {
		t.Fatalf("first-line cost = %v after %d appends, want 99", m.Cost, metaRefreshEvery)
	}
}

// AutoPrune is what the engine calls at the end of a turn: Prune with the
// configured limit, protected current session, at most once per interval.
func TestAutoPruneThrottledAndProtects(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	save := func(id string, i int) {
		if err := s.Save(&Record{ID: id, Model: "claude-opus-5", Messages: []api.Message{{Role: "user", Content: id}}}); err != nil {
			t.Fatal(err)
		}
		s.setIndexUpdatedAt(t, id, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 0; i < 5; i++ {
		save(fmt.Sprintf("a%d", i), i)
	}
	if n, err := s.AutoPrune(0, "a0"); n != 0 || err != nil {
		t.Fatalf("AutoPrune(0) = %d, %v; want a no-op", n, err)
	}
	n, err := s.AutoPrune(2, "a0")
	if err != nil || n != 2 {
		t.Fatalf("AutoPrune(2) = %d, %v; want 2 removed", n, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "a0.jsonl")); err != nil {
		t.Fatalf("the protected session was pruned: %v", err)
	}
	for i := 5; i < 8; i++ {
		save(fmt.Sprintf("a%d", i), i)
	}
	if n, _ := s.AutoPrune(2, "a0"); n != 0 {
		t.Fatalf("second AutoPrune within the interval removed %d", n)
	}
	s.mu.Lock()
	s.lastAutoPrune = time.Now().Add(-autoPruneInterval - time.Second)
	s.mu.Unlock()
	if n, _ := s.AutoPrune(2, "a0"); n != 3 {
		t.Fatalf("AutoPrune after the interval removed %d, want 3", n)
	}
}
