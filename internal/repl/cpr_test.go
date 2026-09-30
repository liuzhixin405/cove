package repl

import (
	"testing"
	"time"
)

func drainCPR() {
	select {
	case <-cprCh:
	default:
	}
}

func expectCPR(t *testing.T, want cursorPos) {
	t.Helper()
	select {
	case pos := <-cprCh:
		if pos != want {
			t.Errorf("reported position = %+v, want %+v", pos, want)
		}
	default:
		t.Fatal("the cursor report did not reach the query channel")
	}
}

// A cursor report that arrived while the history search was open was read as
// "an arrow key": it ended the search and never reached the query, which
// timed out and switched pinning off for the rest of the session.
func TestCursorReportDuringReverseSearchReachesTheQuery(t *testing.T) {
	drainCPR()
	restore := captureStdout(t)
	lr := typed("\x12\x1b[12;5Rb\r\r")
	lr.history = []string{"abc"}
	got, err := lr.editLine()
	restore()
	if err != nil || got != "abc" {
		t.Fatalf("line = %q, %v; want the search to go on and find \"abc\"", got, err)
	}
	expectCPR(t, cursorPos{12, 5})
}

// Inside a bracketed paste every CSI but the closing 201~ was dropped,
// the cursor report with them.
func TestCursorReportDuringBracketedPasteReachesTheQuery(t *testing.T) {
	drainCPR()
	got, err := editOnce(t, "\x1b[200~hi\x1b[4;2Rthere\x1b[201~\r")
	if err != nil || got != "hithere" {
		t.Fatalf("line = %q, %v; want \"hithere\"", got, err)
	}
	expectCPR(t, cursorPos{4, 2})
}

// One unanswered query used to disable pinning for the whole session, though
// the usual cause is a report that arrived where nobody relayed it. A miss
// now only suspends pinning for a while; a late report restores it at once,
// and only several misses in a row give up for good.
func TestCursorQueryMissIsNotPermanent(t *testing.T) {
	defer resetCPRState()
	resetCPRState()
	noteCPRMiss()
	if cprUsable() {
		t.Fatal("pinning retried immediately after a miss")
	}
	cprRetryAt.Store(time.Now().Add(-time.Second).UnixNano())
	if !cprUsable() {
		t.Fatal("pinning never retried after one miss")
	}
	noteCPRMiss()
	deliverCursorReport("3;4") // the late answer
	drainCPR()
	if !cprUsable() {
		t.Fatal("a late report did not restore pinning")
	}
	for i := 0; i < cprMaxMisses; i++ {
		noteCPRMiss()
	}
	cprRetryAt.Store(0)
	if cprUsable() {
		t.Fatal("a terminal that never answers is still queried")
	}
}
