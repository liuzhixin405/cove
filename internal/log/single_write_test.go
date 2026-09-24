package log

import (
	"strings"
	"sync"
	"testing"
)

// writeRecorder keeps every Write call separately, the way the REPL's writer
// sees them: it prints each Write as its own line above the input box.
type writeRecorder struct {
	mu     sync.Mutex
	writes []string
}

func (w *writeRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

// An entry went out as three Writes (prefix, message, newline). The REPL's log
// writer turns every Write into a line of its own, so each warning showed up
// as "[15:04:05.000 WARN]" on one line and the message on the next, and lines
// from two goroutines could interleave between the pieces.
func TestLogEntryIsASingleWrite(t *testing.T) {
	restoreDefaults(t)
	rec := &writeRecorder{}
	SetWriter(rec)
	SetLevel(Info)

	Warnf("mcp server %q did not answer", "atlassian")

	if len(rec.writes) != 1 {
		t.Fatalf("one log entry took %d writes: %q", len(rec.writes), rec.writes)
	}
	got := rec.writes[0]
	if !strings.Contains(got, "WARN]") || !strings.Contains(got, `mcp server "atlassian" did not answer`) || !strings.HasSuffix(got, "\n") {
		t.Fatalf("entry = %q, want prefix, message and newline together", got)
	}
}
