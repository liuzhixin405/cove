package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
)

// Final fix (Minor 1): after the user answers "s" at the limit prompt the
// shell says once that /continue resumes — not three times, and not under a
// "Request failed" prefix (nothing failed; the user chose to stop).
func TestLimitStopMentionsContinueOnce(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	done := make(chan engine.LimitDecision, 1)
	go func() {
		done <- askTurnLimit(engine.LimitStats{Iterations: 200, Reason: engine.LimitReasonIterations, Window: 200})
	}()
	waitForPermInputCh(t) <- "s"
	select {
	case d := <-done:
		if d != engine.LimitStop {
			t.Fatalf("decision = %v, want stop", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("askTurnLimit did not return")
	}

	limitErr := fmt.Errorf("turn: %w", &engine.LimitError{Reason: engine.LimitReasonIterations, Limit: 200})
	out, err := runChatInteraction(context.Background(), stubStreamingRunner{err: limitErr}, "hi")
	if err == nil {
		t.Fatal("expected the limit error")
	}
	// buf is the screen (the stop line is printed there; out is the
	// transcript copy of the same text).
	all := buf.String() + taskErrorHint(err)
	if n := strings.Count(all, "/continue"); n != 1 {
		t.Fatalf("/continue mentioned %d times, want 1:\n%s", n, all)
	}
	if strings.Contains(all+out, "Request failed") {
		t.Fatalf("a stop the user chose is reported as a failure:\n%s", all)
	}

	// Other errors keep the prefix and the resume hint.
	if h := taskErrorHint(fmt.Errorf("api: boom")); !strings.Contains(h, "/continue") {
		t.Fatalf("hint for an ordinary error = %q", h)
	}
}

// The timeout notice no longer repeats /continue either: the stop line that
// follows it carries the hint.
func TestLimitTimeoutNoticeLeavesContinueToStopLine(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	oldInteractive, oldTimeout := replInteractive, limitPromptTimeout
	replInteractive = true
	limitPromptTimeout = 30 * time.Millisecond
	t.Cleanup(func() {
		replInteractive, limitPromptTimeout = oldInteractive, oldTimeout
		repl.ClearPermInputCh()
		termui.SetWriter(nil)
	})
	askTurnLimit(engine.LimitStats{Reason: engine.LimitReasonTime, Window: 60})
	_, _ = runChatInteraction(context.Background(),
		stubStreamingRunner{err: &engine.LimitError{Reason: engine.LimitReasonTime, Limit: 60}}, "hi")
	all := buf.String() + taskErrorHint(&engine.LimitError{Reason: engine.LimitReasonTime, Limit: 60})
	if n := strings.Count(all, "/continue"); n != 1 {
		t.Fatalf("/continue mentioned %d times, want 1:\n%s", n, all)
	}
}
