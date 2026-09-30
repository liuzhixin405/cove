package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

// A corrupt history (e.g. a torn write from the old non-atomic os.WriteFile)
// used to be discarded silently. It is now kept as telemetry.json.bak and the
// recorder starts fresh.
func TestFlushKeepsBackupOfCorruptFile(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()
	if err := os.MkdirAll(filepath.Dir(r.filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	torn := `[{"type":"old","timestamp":"2026-01-01T00:00:00Z"},{"type":"ol`
	if err := os.WriteFile(r.filePath, []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Record("fresh", nil)
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	bak, err := os.ReadFile(r.filePath + ".bak")
	if err != nil {
		t.Fatalf("corrupt file was not backed up: %v", err)
	}
	if string(bak) != torn {
		t.Fatalf("backup = %q, want the corrupt original", bak)
	}
	events, err := readEvents(r.filePath)
	if err != nil || len(events) != 1 || events[0].Type != "fresh" {
		t.Fatalf("events = %v, err = %v", typesOf(events), err)
	}
}

// The buffer used to be cleared before the write, so a failed write lost the
// events it was meant to persist.
func TestFlushKeepsEventsWhenTheWriteFails(t *testing.T) {
	r := newTestRecorder(t)
	r.Enable()
	// A directory where the file should be makes the final rename fail.
	if err := os.MkdirAll(filepath.Join(r.filePath, "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.Record("kept", nil)
	if err := r.Flush(); err == nil {
		t.Fatal("expected Flush to fail")
	}
	if got := r.Stats()["kept"]; got != 1 {
		t.Fatalf("buffered events after a failed Flush = %d, want 1", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(r.filePath))
	for _, e := range entries {
		if fsatomic.IsTempName(e.Name()) || strings.HasSuffix(e.Name(), ".bak") {
			t.Fatalf("unexpected leftover %s", e.Name())
		}
	}
}
