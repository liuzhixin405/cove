package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTokenize_CJKBigrams(t *testing.T) {
	// A run of Chinese used to be one token up to the next punctuation, so
	// "中文回复" could never match a memory that phrased it inside a longer
	// sentence.
	doc := NewBM25(1.2, 0.75)
	doc.Index(0, "用户偏好使用中文回复，提交前运行测试", time.Now())
	doc.Index(1, "The deployment pipeline uses docker.", time.Now())
	res := doc.Search("中文回复", 5)
	if len(res) == 0 || res[0].ID != 0 {
		t.Fatalf("expected the Chinese memory to match, got %+v", res)
	}

	toks := tokenize("中文abc回")
	want := map[string]bool{"中文": true, "abc": true, "回": true}
	if len(toks) != len(want) {
		t.Fatalf("tokens = %q", toks)
	}
	for _, tk := range toks {
		if !want[tk] {
			t.Fatalf("unexpected token %q in %q", tk, toks)
		}
	}
	// Japanese kana and Hangul are segmented the same way.
	for _, s := range []string{"ひらがなテスト", "한국어테스트"} {
		if got := tokenize(s); len(got) != utf8.RuneCountInString(s)-1 {
			t.Fatalf("tokenize(%q) = %q, want overlapping bigrams", s, got)
		}
	}
	// Latin behavior unchanged: short tokens and stopwords dropped.
	if got := tokenize("The go-lang is_ok at"); strings.Join(got, ",") != "go-lang,is_ok" {
		t.Fatalf("latin tokens = %q", got)
	}
}

func TestStore_RelevantMemoriesFor_ChinesePastBudget(t *testing.T) {
	files := map[string]string{
		"pref.md": "用户偏好使用中文回复，提交前运行测试",
	}
	// Push the store past InlineBudgetBytes so ranking kicks in.
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("filler%02d.md", i)] = strings.Repeat("unrelated english filler text about lunch. ", 25)
	}
	s := newTestStore(t, files)
	s.cwd = t.TempDir()
	got := s.RelevantMemoriesFor("请用中文回复")
	if !strings.Contains(got, "pref.md") {
		t.Fatalf("expected pref.md among relevant memories, got %q", got)
	}
}

func TestStore_Search_RecencyUsesFileMtime(t *testing.T) {
	// Every document used to be indexed with updated=now, so the recency
	// term was a constant and a stale memory ranked like a fresh one.
	s := newTestStore(t, map[string]string{
		"old.md": "docker deployment notes",
		"new.md": "docker deployment notes ",
	})
	s.cwd = t.TempDir()
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.dirs[0], "old.md"), old, old); err != nil {
		t.Fatal(err)
	}
	res := s.Search("docker deployment", 5)
	if len(res) != 2 || res[0].Entry.Name != "new.md" {
		t.Fatalf("expected new.md first, got %+v", res)
	}
	for _, e := range s.All() {
		if e.Name == "old.md" && time.Since(e.Updated) < 80*24*time.Hour {
			t.Fatalf("old.md Updated = %v, want its mtime", e.Updated)
		}
	}
}

type recordingEmbedProvider struct {
	calls  [][]string
	errFor func(texts []string) error
}

func (r *recordingEmbedProvider) Dim() int { return 3 }

func (r *recordingEmbedProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.calls = append(r.calls, append([]string(nil), texts...))
	if r.errFor != nil {
		if err := r.errFor(texts); err != nil {
			return nil, err
		}
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func TestStore_Search_EmbedsOnlyCandidatesTruncatedAndBatched(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 100; i++ {
		files[fmt.Sprintf("m%03d.md", i)] = fmt.Sprintf("kubernetes note %d", i)
	}
	files["other.md"] = "totally unrelated lunch preferences"
	files["huge.md"] = "kubernetes " + strings.Repeat("x", 40000)
	s := newTestStore(t, files)
	s.cwd = t.TempDir()
	p := &recordingEmbedProvider{}
	s.EnableRemoteEmbeddings(p)

	res := s.Search("kubernetes", 3)
	if len(res) == 0 {
		t.Fatal("expected results")
	}
	total := 0
	for _, call := range p.calls {
		if len(call) > embedBatchSize {
			t.Fatalf("batch of %d inputs exceeds embedBatchSize %d", len(call), embedBatchSize)
		}
		for _, in := range call {
			total++
			if len(in) > maxEmbedInputBytes {
				t.Fatalf("input of %d bytes sent to the embeddings API", len(in))
			}
			if strings.Contains(in, "lunch") {
				t.Fatal("non-candidate entry was embedded")
			}
		}
	}
	// query + at most topK*2 BM25 candidates.
	if total > 1+3*2 {
		t.Fatalf("embedded %d inputs, want only the BM25 candidates", total)
	}
}

func TestStore_VectorScores_InputSizeErrorDoesNotBackOff(t *testing.T) {
	s := newTestStore(t, map[string]string{"a.md": "hello world"})
	entries := s.All()
	p := &recordingEmbedProvider{errFor: func([]string) error {
		return errors.New("remote embeddings: API error 400: This model's maximum context length is 8192 tokens")
	}}
	s.EnableRemoteEmbeddings(p)
	if got := s.vectorScores(context.Background(), "hello", entries); got != nil {
		t.Fatalf("expected nil scores on failure, got %v", got)
	}
	p.errFor = nil
	if got := s.vectorScores(context.Background(), "hello", entries); got == nil {
		t.Fatal("an input-size error must not start the 5-minute backoff")
	}
}

func TestClipForEmbedding_CJKTokenBudget(t *testing.T) {
	in := strings.Repeat("中", 20000)
	out := clipForEmbedding(in)
	if n := utf8.RuneCountInString(out); n >= 8000 {
		t.Fatalf("CJK input clipped to %d runes; would exceed an 8k-token input limit", n)
	}
	if !utf8.ValidString(out) {
		t.Fatal("clipped output is not valid UTF-8")
	}
	if short := "short text"; clipForEmbedding(short) != short {
		t.Fatal("short input must be unchanged")
	}
}
