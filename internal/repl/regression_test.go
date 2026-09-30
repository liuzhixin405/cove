package repl

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A completion that turned a line with CJK text into a longer one set the
// cursor to the byte length of the result (16 for "请看 @internal/", which
// is 12 runes); the redraw then indexed the buffer past its end and the
// program exited with a panic.
func TestCompletionCursorCountsRunesNotBytes(t *testing.T) {
	complete := func(in string) []string {
		if strings.HasPrefix(in, "请看 @inter") {
			return []string{"请看 @internal/"}
		}
		return nil
	}
	restore := captureStdout(t)
	defer restore()
	// A Tab with more input buffered behind it is pasted text, so the key
	// is driven directly rather than through editLine.
	lr := New(complete)
	buf := []rune("请看 @inter")
	cursor := len(buf)
	lr.complete(&buf, &cursor)
	if string(buf) != "请看 @internal/" || cursor != len(buf) {
		t.Fatalf("buf %q, cursor %d (len %d)", string(buf), cursor, len(buf))
	}
}

// Cycling through candidates with Tab set the cursor the same byte-wise way.
func TestCompletionCycleCursorCountsRunesNotBytes(t *testing.T) {
	complete := func(in string) []string {
		if strings.HasPrefix(in, "请看 @") {
			return []string{"请看 @档", "请看 @案"}
		}
		return nil
	}
	restore := captureStdout(t)
	defer restore()
	lr := New(complete)
	buf := []rune("请看 @")
	cursor := len(buf)
	for _, want := range []string{"请看 @", "请看 @档", "请看 @案"} {
		lr.complete(&buf, &cursor)
		if string(buf) != want || cursor != len(buf) {
			t.Fatalf("buf %q (want %q), cursor %d (len %d)", string(buf), want, cursor, len(buf))
		}
	}
}

// The common prefix was trimmed a byte at a time, so two candidates that
// share the first two bytes of their next character (档 E6 A1 A3, 案 E6 A1
// 88) left half a character in the input.
func TestCommonPrefixKeepsWholeRunes(t *testing.T) {
	got := commonPrefix([]string{"@档案", "@案卷"})
	if got != "@" || !utf8.ValidString(got) {
		t.Fatalf("commonPrefix = %q, want %q", got, "@")
	}
	if got := commonPrefix([]string{"/api-key", "/attach"}); got != "/a" {
		t.Fatalf("commonPrefix = %q, want /a", got)
	}
}

// A cursor past the end of the buffer (a caller's mistake) must not crash
// the editor.
func TestRedrawClampsACursorPastTheEnd(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	lr.redraw([]rune("请看"), 6)
	lr.redraw([]rune("请看"), -1)
	restore()
	if _, _, _, start := inputDisplayWindow([]rune("ab"), 9, 10); start != 0 {
		t.Fatalf("start = %d", start)
	}
}

// Ctrl+D on an empty line while the input row is pinned used to return
// ErrExit with the scroll region still set: the process exited and the
// user's shell scrolled inside a region two rows short of the window.
func TestCtrlDWhilePinnedResetsTheScrollRegion(t *testing.T) {
	restore := captureStdout(t)
	lr := typed("\x04")
	withPinned(t, lr, 80, 50)
	_, err := lr.editLine()
	out := restore()
	if err != ErrExit {
		t.Fatalf("err = %v, want ErrExit", err)
	}
	if !strings.Contains(out, unpinSequence(50)) {
		t.Fatalf("scroll region not reset on exit: %q", out)
	}
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if pinned || streamingActive {
		t.Errorf("pinned %v, streaming %v after exit", pinned, streamingActive)
	}
}

// The same holds when stdin ends (EOF) while pinned.
func TestEndOfInputWhilePinnedResetsTheScrollRegion(t *testing.T) {
	restore := captureStdout(t)
	lr := typed("")
	withPinned(t, lr, 80, 50)
	_, err := lr.editLine()
	out := restore()
	if err != ErrExit {
		t.Fatalf("err = %v, want ErrExit", err)
	}
	if !strings.Contains(out, unpinSequence(50)) {
		t.Fatalf("scroll region not reset on exit: %q", out)
	}
}

// The hint after the input was given the room left of the cursor, but it is
// printed after the whole visible text: with the cursor moved left the row
// overflowed by the text right of the cursor and soft-wrapped.
func TestHintFitsWithTheCursorInsideTheText(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	text := []rune(strings.Repeat("a", 60))
	lr.activeHint = "\x1b[90m  " + strings.Repeat("h", 60) + "\x1b[0m"
	consoleMu.Lock()
	lr.redrawLocked(text, 10)
	consoleMu.Unlock()
	out := restore()
	// The first draw of the row runs up to the "\r" that repositions the
	// cursor; it must fit in the 80-column fallback width with a column to
	// spare.
	row := out[strings.LastIndex(out, "\x1b[2K")+len("\x1b[2K"):]
	row = row[:strings.Index(row, "\r")]
	if w := visibleCells(row); w > 79 {
		t.Fatalf("row is %d cells wide, over 79: %q", w, row)
	}
}

// Ctrl+R kept the previous match when the extended query matched nothing:
// with "git status" in the history, "gitx" still showed it and Enter took it.
func TestCtrlRWithNoMatchDoesNotAcceptAStaleMatch(t *testing.T) {
	restore := captureStdout(t)
	lr := typed("\x12gitx\r\r")
	lr.history = []string{"git status"}
	lr.histIdx = len(lr.history)
	got, err := lr.editLine()
	out := restore()
	if err != nil || got != "" {
		t.Fatalf("got %q, %v; want the original (empty) line", got, err)
	}
	if !strings.Contains(out, "无匹配") {
		t.Errorf("failing search not shown: %q", out)
	}
}

// Deleting back to a query that matches again finds the match again.
func TestCtrlRRecoversAfterBackspace(t *testing.T) {
	restore := captureStdout(t)
	lr := typed("\x12gitx\x7f\r\r")
	lr.history = []string{"git status"}
	lr.histIdx = len(lr.history)
	got, err := lr.editLine()
	restore()
	if err != nil || got != "git status" {
		t.Fatalf("got %q, %v", got, err)
	}
}
