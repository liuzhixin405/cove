package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestStore creates a Store rooted at a temp directory, bypassing
// NewStore()'s dependency on the real user home directory.
func newTestStore(t *testing.T, files map[string]string) *Store {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("failed to write fixture %s: %v", name, err)
		}
	}
	return &Store{dirs: []string{dir}}
}

type fakeEmbedProvider struct {
	calls  int
	vecFor func(text string) []float32
	err    error
}

func (f *fakeEmbedProvider) Dim() int { return 3 }

func (f *fakeEmbedProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, txt := range texts {
		out[i] = f.vecFor(txt)
	}
	return out, nil
}

func TestStore_Search_PureBM25WhenEmbeddingsDisabled(t *testing.T) {
	s := newTestStore(t, map[string]string{
		"a.md": "The deployment pipeline uses docker and kubernetes for releases.",
		"b.md": "Unrelated notes about lunch preferences and scheduling.",
	})

	results := s.Search("docker kubernetes deployment", 5)
	if len(results) == 0 {
		t.Fatal("expected at least one BM25 match")
	}
	if results[0].Entry.Name != "a.md" {
		t.Fatalf("expected a.md to rank first for a matching query, got %s", results[0].Entry.Name)
	}
}

func TestStore_VectorScores_NilWhenDisabled(t *testing.T) {
	s := newTestStore(t, map[string]string{"a.md": "hello world"})
	entries := s.All()
	if got := s.vectorScores(context.Background(), "hello", entries); got != nil {
		t.Fatalf("expected nil vector scores when embeddings are not enabled, got %v", got)
	}
}

func TestStore_VectorScores_BlendsWhenEnabled(t *testing.T) {
	s := newTestStore(t, map[string]string{
		"a.md": "alpha content",
		"b.md": "beta content",
	})
	entries := s.All()

	// Query vector matches "b.md" exactly (cosine 1.0), "a.md" not at all (0.0).
	provider := &fakeEmbedProvider{
		vecFor: func(text string) []float32 {
			if text == "beta content" {
				return []float32{1, 0, 0}
			}
			if text == "alpha content" {
				return []float32{0, 1, 0}
			}
			return []float32{1, 0, 0} // query
		},
	}
	s.EnableRemoteEmbeddings(provider)

	scores := s.vectorScores(context.Background(), "query", entries)
	if scores == nil {
		t.Fatal("expected non-nil vector scores when embeddings are enabled")
	}

	var aIdx, bIdx = -1, -1
	for i, e := range entries {
		switch e.Name {
		case "a.md":
			aIdx = i
		case "b.md":
			bIdx = i
		}
	}
	if aIdx < 0 || bIdx < 0 {
		t.Fatal("fixture entries not found")
	}
	if scores[bIdx] <= scores[aIdx] {
		t.Fatalf("expected b.md's vector score (%v) to exceed a.md's (%v)", scores[bIdx], scores[aIdx])
	}

	// Second call should reuse the cache for entries (only the query needs
	// re-embedding), not re-embed everything from scratch.
	callsAfterFirst := provider.calls
	_ = s.vectorScores(context.Background(), "query again", entries)
	if provider.calls != callsAfterFirst+1 {
		t.Fatalf("expected exactly one more Embed call (query only, entries cached), calls went %d -> %d", callsAfterFirst, provider.calls)
	}
}

func TestStore_VectorScores_BacksOffAfterFailure(t *testing.T) {
	s := newTestStore(t, map[string]string{"a.md": "hello world"})
	entries := s.All()

	provider := &fakeEmbedProvider{err: errors.New("endpoint unreachable")}
	s.EnableRemoteEmbeddings(provider)

	if got := s.vectorScores(context.Background(), "hello", entries); got != nil {
		t.Fatalf("expected nil scores on failure, got %v", got)
	}
	if provider.calls != 1 {
		t.Fatalf("expected exactly 1 call before backoff kicks in, got %d", provider.calls)
	}

	// Immediately retrying should NOT call the provider again — it's
	// backing off after the failure.
	if got := s.vectorScores(context.Background(), "hello", entries); got != nil {
		t.Fatalf("expected nil scores while backing off, got %v", got)
	}
	if provider.calls != 1 {
		t.Fatalf("expected no additional call while backing off, got %d calls", provider.calls)
	}
}

func TestStore_Search_StillWorksWithEmbeddingsEnabledButFailing(t *testing.T) {
	// Search must degrade gracefully to pure BM25 ranking if the embeddings
	// endpoint is enabled but broken — never error out or return nothing.
	s := newTestStore(t, map[string]string{
		"a.md": "The deployment pipeline uses docker and kubernetes for releases.",
	})
	s.EnableRemoteEmbeddings(&fakeEmbedProvider{err: errors.New("boom")})

	results := s.Search("docker kubernetes deployment", 5)
	if len(results) == 0 {
		t.Fatal("expected BM25 fallback results despite a broken embeddings provider")
	}
}
