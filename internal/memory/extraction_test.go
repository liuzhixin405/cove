package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// RecordExtraction is what makes background learning visible: /memory stats
// and /doctor report when the last extraction ran and how many memories it
// saved, and a fresh process (cove doctor) reads the same record from disk.
func TestRecordExtractionShowsInStats(t *testing.T) {
	s := newTestStore(t, map[string]string{"a.md": "alpha"})
	if st := s.Stats(); !st.LastExtractedAt.IsZero() || st.LastExtractedCount != 0 {
		t.Fatalf("fresh store reports an extraction: %+v", st)
	}
	before := time.Now().Add(-time.Second)
	s.RecordExtraction(2)
	st := s.Stats()
	if st.LastExtractedAt.Before(before) || st.LastExtractedCount != 2 {
		t.Fatalf("Stats after RecordExtraction(2) = %+v", st)
	}
	if st.FileCount != 1 {
		t.Fatalf("the extraction record was counted as a memory: FileCount = %d", st.FileCount)
	}

	again := &Store{dirs: s.dirs}
	if got := again.Stats(); got.LastExtractedCount != 2 || got.LastExtractedAt.IsZero() {
		t.Fatalf("another Store over the same dir does not see the record: %+v", got)
	}
}

// An extraction that saved nothing still ran: the time moves, the count is 0.
func TestRecordExtractionZero(t *testing.T) {
	s := newTestStore(t, nil)
	s.RecordExtraction(3)
	s.RecordExtraction(0)
	if st := s.Stats(); st.LastExtractedCount != 0 || st.LastExtractedAt.IsZero() {
		t.Fatalf("Stats = %+v, want count 0 with a time", st)
	}
}

// The extract runner writes memory files behind the store's back; recording
// the extraction must drop the cached entries so the new files show up.
func TestRecordExtractionInvalidatesCache(t *testing.T) {
	s := newTestStore(t, map[string]string{"a.md": "alpha"})
	s.cacheTTL = time.Hour
	if n := len(s.All()); n != 1 {
		t.Fatalf("All() = %d entries", n)
	}
	if err := os.WriteFile(filepath.Join(s.dirs[0], "b.md"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.RecordExtraction(1)
	if n := len(s.All()); n != 2 {
		t.Fatalf("All() after RecordExtraction = %d entries, want 2", n)
	}
}

func TestRecordExtractionInDirIsReadByStore(t *testing.T) {
	dir := t.TempDir()
	RecordExtractionIn(dir, 4)
	s := &Store{dirs: []string{dir}}
	if st := s.Stats(); st.LastExtractedCount != 4 {
		t.Fatalf("Stats = %+v, want count 4", st)
	}
}
