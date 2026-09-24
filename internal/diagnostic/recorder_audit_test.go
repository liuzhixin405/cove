package diagnostic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useRuntimeLog points persistence at path for one test. Persistence is
// normally off under `go test` so fixtures cannot reach ~/.cove/errors.log.
func useRuntimeLog(t *testing.T, path string) {
	t.Helper()
	runtimePathOnce.Do(func() {})
	old := runtimePath
	runtimePath = path
	t.Cleanup(func() { runtimePath = old })
}

// errors.log is appended to on every Warn/Error from any goroutine and was
// never trimmed; LoadRuntimeLog (used by /diagnose errors) then read the whole
// file into memory. Several MB of old noise also buried the recent problems.
func TestRuntimeLogDoesNotGrowWithoutBound(t *testing.T) {
	p := filepath.Join(t.TempDir(), "errors.log")
	useRuntimeLog(t, p)
	line := `{"time":"2026-01-01T00:00:00Z","severity":1,"category":"engine","message":"old noise"}` + "\n"
	const oldSize = 5 << 20
	if err := os.WriteFile(p, []byte(strings.Repeat(line, oldSize/len(line)+1)), 0o644); err != nil {
		t.Fatal(err)
	}

	RecordRuntime(SevWarning, CatEngine, "fresh problem")

	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= oldSize {
		t.Fatalf("errors.log is %d bytes after a write; it keeps growing", info.Size())
	}
	events := LoadRuntimeLog()
	if len(events) == 0 || events[len(events)-1].Message != "fresh problem" {
		t.Fatalf("latest event missing from the current log: %d events", len(events))
	}
}
