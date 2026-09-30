package main

import (
	"strings"
	"testing"
)

// stdout and stderr of a command are copied by separate goroutines and used
// to share one sanitiser, so an escape sequence held back at the end of a
// stdout chunk was completed by the start of a stderr chunk: "\x1b[3" + "1m"
// turned the error output red (or any sequence the two halves spelled).
func TestToolProgressKeepsStreamsApart(t *testing.T) {
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.toolProgress("bash", "out\x1b[3")
	p.toolStderrProgress("bash", "1mERR\n")
	p.stop()
	if out := buf.String(); strings.Contains(out, "\x1b[31m") {
		t.Fatalf("a sequence was assembled across stdout and stderr: %q", out)
	}
}

// A failed attempt's held-back reasoning bytes were only flushed by stop(),
// so the retry's first reasoning chunk completed them.
func TestBeginAttemptFlushesHeldReasoning(t *testing.T) {
	old := showReasoning
	showReasoning = true
	t.Cleanup(func() { showReasoning = old })
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.beginAttempt()
	p.reasoning("think\x1b[3")
	p.beginAttempt()
	p.reasoning("1mretry")
	p.stop()
	if out := buf.String(); strings.Contains(out, "\x1b[31m") {
		t.Fatalf("held reasoning of the failed attempt joined the retry: %q", out)
	}
}
