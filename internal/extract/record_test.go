package extract

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/memory"
)

type fakeRecorder struct {
	mu    sync.Mutex
	calls []int
}

func (f *fakeRecorder) RecordExtraction(n int) {
	f.mu.Lock()
	f.calls = append(f.calls, n)
	f.mu.Unlock()
}

// A finished extraction is reported to the memory store, so /memory stats and
// /doctor can show when background learning last ran and what it saved.
func TestExtractRecordsCompletedRun(t *testing.T) {
	p := &fakeProvider{response: memoryBlock("facts.md", "write", "the build uses go 1.25")}
	r, _ := newTestRunner(t, p)
	rec := &fakeRecorder{}
	r.SetRecorder(rec)

	r.Extract(context.Background(), conversation(6))

	if len(rec.calls) != 1 || rec.calls[0] != 1 {
		t.Fatalf("RecordExtraction calls = %v, want [1]", rec.calls)
	}
}

// A run that found nothing still ran and is recorded with 0.
func TestExtractRecordsEmptyRun(t *testing.T) {
	p := &fakeProvider{response: "NONE"}
	r, _ := newTestRunner(t, p)
	rec := &fakeRecorder{}
	r.SetRecorder(rec)

	r.Extract(context.Background(), conversation(6))

	if len(rec.calls) != 1 || rec.calls[0] != 0 {
		t.Fatalf("RecordExtraction calls = %v, want [0]", rec.calls)
	}
}

// A failed API call, or a conversation too short to look at, is not a run.
func TestExtractDoesNotRecordFailureOrSkip(t *testing.T) {
	p := &fakeProvider{err: errors.New("boom")}
	r, _ := newTestRunner(t, p)
	rec := &fakeRecorder{}
	r.SetRecorder(rec)

	r.Extract(context.Background(), conversation(2))
	r.Extract(context.Background(), conversation(6))

	if len(rec.calls) != 0 {
		t.Fatalf("RecordExtraction calls = %v, want none", rec.calls)
	}
}

// Without a recorder the record still lands in the memory directory, where a
// memory.Store over that directory reads it.
func TestExtractWithoutRecorderWritesRecordFile(t *testing.T) {
	p := &fakeProvider{response: memoryBlock("facts.md", "write", "the build uses go 1.25")}
	r, dir := newTestRunner(t, p)

	r.Extract(context.Background(), conversation(6))

	st := memory.NewStoreForDirs(dir).Stats()
	if st.LastExtractedCount != 1 || st.LastExtractedAt.IsZero() {
		t.Fatalf("Stats = %+v, want the run recorded", st)
	}
}
