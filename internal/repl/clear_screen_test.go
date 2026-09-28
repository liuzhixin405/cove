package repl

import (
	"bufio"
	"strings"
	"testing"
)

func TestClearScreenClearsAndRedrawsTheInputLine(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withActiveReader(t, lr)
	consoleMu.Lock()
	lr.renderBuf = []rune("half typed")
	lr.renderCursor = len(lr.renderBuf)
	consoleMu.Unlock()

	ok := ClearScreen()
	out := restore()

	if !ok {
		t.Fatal("ClearScreen refused while idle")
	}
	i := strings.Index(out, clearScreenSeq)
	if i < 0 {
		t.Fatalf("no clear sequence: %q", out)
	}
	if !strings.Contains(out[i:], "half typed") {
		t.Fatalf("input line not redrawn after clearing: %q", out)
	}
}

// Clearing mid-stream would wipe the pinned input row and its scroll region.
func TestClearScreenRefusesWhileStreaming(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withActiveReader(t, lr)
	consoleMu.Lock()
	streamingActive = true
	consoleMu.Unlock()

	ok := ClearScreen()
	out := restore()

	if ok || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("cleared during streaming: ok=%v out=%q", ok, out)
	}
}

func TestCtrlLClearsAndKeepsTheTypedText(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	lr.rawReader = bufio.NewReader(strings.NewReader("abc\x0cd\r"))
	withActiveReader(t, lr)

	line, err := lr.editLine()
	out := restore()

	if err != nil || line != "abcd" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if !strings.Contains(out, clearScreenSeq) {
		t.Fatalf("Ctrl+L did not clear: %q", out)
	}
}
