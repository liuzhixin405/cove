package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// isolateHome points os.UserHomeDir at a temp directory so no test can create
// or touch ~/.cove/telemetry.json. USERPROFILE is what os.UserHomeDir reads on
// Windows; HOME is used elsewhere.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// newTestRecorder builds a recorder whose file lives in a temp directory.
// The recorder exposes no way to set filePath, so the in-package test sets it
// directly (see the report: this is a testability gap in the public API).
func newTestRecorder(t *testing.T) *Recorder {
	t.Helper()
	home := isolateHome(t)
	r := NewRecorder()
	r.filePath = filepath.Join(home, "telemetry-under-test", "telemetry.json")
	return r
}

func TestNewRecorderStartsDisabledAndIgnoresRecords(t *testing.T) {
	isolateHome(t)

	r := NewRecorder()
	if r.enabled {
		t.Fatal("NewRecorder() starts enabled; telemetry is documented as opt-in only")
	}

	r.Record("boot", map[string]any{"k": "v"})
	r.RecordUsage("m", 1, 2, 0.5, time.Second)
	r.RecordToolCall("Bash", true, time.Second)

	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("a disabled recorder collected %v, want nothing", got)
	}
	if n := len(r.events); n != 0 {
		t.Fatalf("a disabled recorder buffered %d events", n)
	}
}

func TestNewRecorderUsesHomeCoveTelemetryPath(t *testing.T) {
	home := isolateHome(t)

	r := NewRecorder()

	want := filepath.Join(home, ".cove", "telemetry.json")
	if r.filePath != want {
		t.Fatalf("filePath = %q, want %q", r.filePath, want)
	}
	// Constructing a recorder must not create anything on disk yet.
	if _, err := os.Stat(filepath.Join(home, ".cove")); !os.IsNotExist(err) {
		t.Fatalf("NewRecorder created %s eagerly (stat err = %v)", filepath.Join(home, ".cove"), err)
	}
}

func TestEnableThenRecordCollectsEventAndDisableStops(t *testing.T) {
	r := newTestRecorder(t)

	r.Enable()
	r.Record("a", map[string]any{"n": 1})
	r.Record("a", nil)
	r.Record("b", nil)

	if got, want := r.Stats(), map[string]int{"a": 2, "b": 1}; !sameCounts(got, want) {
		t.Fatalf("Stats() = %v, want %v", got, want)
	}

	r.Disable()
	r.Record("c", nil)
	if got := r.Stats()["c"]; got != 0 {
		t.Fatalf("Record after Disable stored %d events of type c, want 0", got)
	}
	// Disable must not discard what was already collected.
	if got, want := r.Stats(), map[string]int{"a": 2, "b": 1}; !sameCounts(got, want) {
		t.Fatalf("after Disable, Stats() = %v, want %v", got, want)
	}

	r.Enable()
	r.Record("c", nil)
	if got := r.Stats()["c"]; got != 1 {
		t.Fatalf("Record after re-Enable stored %d events of type c, want 1", got)
	}
}

func TestRecordStoresTypeDataAndTimestamp(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	before := time.Now()
	payload := map[string]any{"detail": "x"}
	r.Record("custom", payload)
	after := time.Now()

	if len(r.events) != 1 {
		t.Fatalf("got %d events, want 1", len(r.events))
	}
	e := r.events[0]
	if e.Type != "custom" {
		t.Errorf("Type = %q, want %q", e.Type, "custom")
	}
	if e.Timestamp.Before(before) || e.Timestamp.After(after) {
		t.Errorf("Timestamp %v outside [%v, %v]", e.Timestamp, before, after)
	}
	got, ok := e.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data has type %T, want map[string]any", e.Data)
	}
	if got["detail"] != "x" {
		t.Errorf("Data = %v, want detail=x", got)
	}
}

func TestRecordUsagePutsDocumentedKeysInData(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	r.RecordUsage("claude-sonnet", 1200, 340, 0.0421, 1500*time.Millisecond)

	if len(r.events) != 1 {
		t.Fatalf("got %d events, want 1", len(r.events))
	}
	if r.events[0].Type != "usage" {
		t.Fatalf("event type = %q, want %q", r.events[0].Type, "usage")
	}
	data, ok := r.events[0].Data.(map[string]any)
	if !ok {
		t.Fatalf("Data has type %T, want map[string]any", r.events[0].Data)
	}
	want := map[string]any{
		"model":       "claude-sonnet",
		"tokens_in":   1200,
		"tokens_out":  340,
		"cost":        0.0421,
		"duration_ms": int64(1500),
	}
	assertExactData(t, data, want)
}

func TestRecordToolCallPutsDocumentedKeysInData(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	r.RecordToolCall("Bash", false, 250*time.Millisecond)

	if r.events[0].Type != "tool_call" {
		t.Fatalf("event type = %q, want %q", r.events[0].Type, "tool_call")
	}
	data, ok := r.events[0].Data.(map[string]any)
	if !ok {
		t.Fatalf("Data has type %T, want map[string]any", r.events[0].Data)
	}
	want := map[string]any{
		"tool":        "Bash",
		"success":     false,
		"duration_ms": int64(250),
	}
	assertExactData(t, data, want)
}

// TestRecordBufferDropsOldestAtCap verifies the in-memory cap keeps the NEWEST
// events: an unbounded buffer would be a slow memory leak, and a cap that
// dropped the newest events would hide the most recent activity.
func TestRecordBufferDropsOldestAtCap(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	const total = 1100
	for i := 0; i < total; i++ {
		r.Record("e", i)
	}

	if len(r.events) > 1000 {
		t.Fatalf("buffer grew to %d events, want at most 1000", len(r.events))
	}
	if len(r.events) == total {
		t.Fatalf("buffer is unbounded: holds all %d events", total)
	}

	// The retained window must be the tail, contiguous and in order.
	first := r.events[0].Data.(int)
	for i, e := range r.events {
		if got := e.Data.(int); got != first+i {
			t.Fatalf("event %d has payload %d, want %d (retained window is not contiguous)", i, got, first+i)
		}
	}
	if last := r.events[len(r.events)-1].Data.(int); last != total-1 {
		t.Fatalf("newest retained event is %d, want %d: the cap dropped the newest instead of the oldest", last, total-1)
	}
	if first == 0 {
		t.Fatalf("oldest event (0) was never dropped although %d events were recorded", total)
	}
}

func TestFlushWritesReadableFileAndClearsBuffer(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()
	r.RecordUsage("m1", 10, 20, 0.5, 100*time.Millisecond)
	r.Record("tool_call", map[string]any{"tool": "Read"})

	if err := r.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	events, err := readEvents(r.filePath)
	if err != nil {
		t.Fatalf("readEvents(%q) error = %v", r.filePath, err)
	}
	if len(events) != 2 {
		t.Fatalf("file holds %d events, want 2", len(events))
	}
	if events[0].Type != "usage" || events[1].Type != "tool_call" {
		t.Errorf("file holds types %q,%q, want usage,tool_call", events[0].Type, events[1].Type)
	}
	if events[0].Timestamp.IsZero() {
		t.Error("flushed event lost its timestamp")
	}
	data, ok := events[0].Data.(map[string]any)
	if !ok || data["model"] != "m1" {
		t.Errorf("flushed usage data = %v, want model=m1", events[0].Data)
	}

	// Flushing must drain the in-memory buffer, otherwise the next Flush
	// duplicates every event.
	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("Stats() after Flush = %v, want empty", got)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("second Flush() error = %v", err)
	}
	events, err = readEvents(r.filePath)
	if err != nil {
		t.Fatalf("readEvents after second flush: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("after a second Flush the file holds %d events, want 2 (duplicated)", len(events))
	}
}

func TestFlushAppendsToExistingFile(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	r.Record("first", nil)
	if err := r.Flush(); err != nil {
		t.Fatalf("first Flush: %v", err)
	}
	r.Record("second", nil)
	if err := r.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}

	events, err := readEvents(r.filePath)
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) != 2 || events[0].Type != "first" || events[1].Type != "second" {
		t.Fatalf("file holds %d events %v, want [first second] in order", len(events), typesOf(events))
	}
}

func TestFlushWithNoEventsWritesNothing(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	if err := r.Flush(); err != nil {
		t.Fatalf("Flush() on an empty recorder returned %v, want nil", err)
	}
	if _, err := os.Stat(r.filePath); !os.IsNotExist(err) {
		t.Fatalf("Flush() with no events created %q (stat err = %v)", r.filePath, err)
	}
	if _, err := os.Stat(filepath.Dir(r.filePath)); !os.IsNotExist(err) {
		t.Fatalf("Flush() with no events created the parent directory")
	}
}

// TestFlushCapsFileAtFiveThousandKeepingNewest exercises the on-disk cap, which
// trims from the front so the file keeps the most recent history.
func TestFlushCapsFileAtFiveThousandKeepingNewest(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	existing := make([]Event, 4900)
	for i := range existing {
		existing[i] = Event{Type: fmt.Sprintf("old-%d", i), Timestamp: time.Unix(int64(i), 0).UTC()}
	}
	if err := os.MkdirAll(filepath.Dir(r.filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.filePath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 200; i++ {
		r.Record(fmt.Sprintf("new-%d", i), nil)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events, err := readEvents(r.filePath)
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) != 5000 {
		t.Fatalf("file holds %d events, want it capped at 5000", len(events))
	}
	if events[0].Type != "old-100" {
		t.Errorf("oldest kept event = %q, want old-100 (the first 100 should be trimmed)", events[0].Type)
	}
	if events[len(events)-1].Type != "new-199" {
		t.Errorf("newest kept event = %q, want new-199", events[len(events)-1].Type)
	}
}

// TestFlushKeepsEventsWhenTheDirectoryCannotBeCreated makes sure a failed flush
// reports the error and does not silently throw away the buffered events.
func TestFlushKeepsEventsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()
	r.Record("kept", nil)

	// A regular file where a directory is required makes MkdirAll fail on every
	// platform.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.filePath = filepath.Join(blocker, "nested", "telemetry.json")

	if err := r.Flush(); err == nil {
		t.Fatal("Flush() to an uncreatable directory returned nil error")
	}
	if got, want := r.Stats(), map[string]int{"kept": 1}; !sameCounts(got, want) {
		t.Fatalf("after a failed Flush, Stats() = %v, want %v (events were dropped)", got, want)
	}
}

func TestStatsAggregatesByEventTypeAndReturnsACopy(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	for i := 0; i < 3; i++ {
		r.RecordToolCall("Bash", true, time.Millisecond)
	}
	r.RecordUsage("m", 1, 1, 0, time.Millisecond)
	r.Record("custom", nil)
	r.Record("custom", nil)

	want := map[string]int{"tool_call": 3, "usage": 1, "custom": 2}
	got := r.Stats()
	if !sameCounts(got, want) {
		t.Fatalf("Stats() = %v, want %v", got, want)
	}

	// Mutating the returned map must not corrupt later reads.
	got["tool_call"] = 999
	delete(got, "usage")
	if again := r.Stats(); !sameCounts(again, want) {
		t.Fatalf("Stats() = %v after the caller mutated a previous result, want %v", again, want)
	}
}

// TestEnableDisableRecordAndFlushAreConcurrencySafe must be clean under -race:
// Record is called from background goroutines while /telemetry style commands
// toggle the flag.
func TestEnableDisableRecordAndFlushAreConcurrencySafe(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()

	const iterations = 300
	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				r.Record("concurrent", i)
				r.RecordToolCall("Bash", i%2 == 0, time.Millisecond)
				r.RecordUsage("m", i, i, float64(i), time.Millisecond)
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			r.Enable()
			r.Disable()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			_ = r.Stats()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations/10; i++ {
			if err := r.Flush(); err != nil {
				t.Errorf("concurrent Flush: %v", err)
				return
			}
		}
	}()

	close(start)
	wg.Wait()

	// Leave the recorder enabled and prove it still works after the storm.
	r.Enable()
	r.Record("after", nil)
	if got := r.Stats()["after"]; got != 1 {
		t.Fatalf("recorder is broken after concurrent use: Stats()[after] = %d, want 1", got)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("final Flush: %v", err)
	}
	events, err := readEvents(r.filePath)
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events reached disk across the concurrent flushes")
	}
}

func TestReadEventsRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readEvents(path); err == nil {
		t.Fatal("readEvents on a corrupt file returned nil error")
	}
	if _, err := readEvents(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("readEvents on a missing file returned nil error")
	}
}

// TestFlushOverExistingCorruptFileStillPersists documents that a corrupt
// history is discarded rather than blocking new telemetry.
func TestFlushOverExistingCorruptFileStillPersists(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()
	if err := os.MkdirAll(filepath.Dir(r.filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.filePath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.Record("fresh", nil)
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush over a corrupt file: %v", err)
	}

	events, err := readEvents(r.filePath)
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != "fresh" {
		t.Fatalf("file holds %v, want exactly [fresh]", typesOf(events))
	}
}

func sameCounts(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func typesOf(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// assertExactData compares an event payload against the expected key set,
// failing on both missing and unexpected keys so a renamed key is caught.
func assertExactData(t *testing.T, got, want map[string]any) {
	t.Helper()
	for k, v := range want {
		actual, ok := got[k]
		if !ok {
			t.Errorf("event data is missing key %q", k)
			continue
		}
		if actual != v {
			t.Errorf("event data[%q] = %v (%T), want %v (%T)", k, actual, actual, v, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("event data has unexpected key %q = %v", k, got[k])
		}
	}
}
