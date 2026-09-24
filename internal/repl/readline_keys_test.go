package repl

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// captureStdout swaps os.Stdout (every write in this package goes through
// fmt.Print) for a pipe and returns a function that restores it and hands back
// what was written.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	restored := false
	restore := func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stdout = old
		w.Close()
		out := <-done
		r.Close()
		return out
	}
	t.Cleanup(func() { restore() })
	return restore
}

// typed builds a LineReader whose raw input is the given key bytes, the way
// the terminal delivers them in raw mode.
func typed(keys string) *LineReader {
	lr := New(nil)
	lr.rawReader = bufio.NewReader(strings.NewReader(keys))
	return lr
}

func editOnce(t *testing.T, keys string) (string, error) {
	t.Helper()
	restore := captureStdout(t)
	line, err := typed(keys).editLine()
	restore()
	return line, err
}

// Only a bare ESC [ <letter> was understood. Any sequence with parameters —
// Ctrl/Shift/Alt + arrow ("\x1b[1;5D", which Windows Terminal sends for
// Ctrl+Left), Home/End as "\x1b[1~"/"\x1b[4~", Insert, F5 — stopped after the
// first parameter byte and typed the rest into the input as text (";5D", "~").
func TestEscapeSequencesNeverTypeGarbage(t *testing.T) {
	tests := []struct {
		name, keys, want string
	}{
		{"ctrl+left moves left", "ab\x1b[1;5Dx\r", "axb"},
		{"shift+right moves right", "ab\x1b[D\x1b[D\x1b[1;2Cx\r", "axb"},
		{"home as CSI 1~", "bc\x1b[1~a\r", "abc"},
		{"home as CSI 7~", "bc\x1b[7~a\r", "abc"},
		{"end as CSI 4~", "ac\x1b[D\x1b[D\x1b[4~d\r", "acd"},
		{"home as SS3 H", "bc\x1bOHa\r", "abc"},
		{"end as SS3 F", "ab\x1b[D\x1b[D\x1bOFc\r", "abc"},
		{"insert is ignored", "ab\x1b[2~\r", "ab"},
		{"F5 is ignored", "ab\x1b[15~\r", "ab"},
		{"F1 as SS3 is ignored", "ab\x1bOP\r", "ab"},
		{"ctrl+delete deletes", "abc\x1b[D\x1b[3;5~\r", "ab"},
		{"focus event is ignored", "ab\x1b[I\x1b[O\r", "ab"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := editOnce(t, tc.keys)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("typed %q, got line %q, want %q", tc.keys, got, tc.want)
			}
		})
	}
}

// Every newline in raw mode must be written as \r\n; a bare \n moves down
// without returning to column 0 and staircases the rest of the screen.
func assertNoBareLF(t *testing.T, out string) {
	t.Helper()
	for i := 0; i < len(out); i++ {
		if out[i] == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("bare \\n written in raw mode at byte %d: %q", i, out)
		}
	}
}

// Pasting several lines submitted each newline as its own message (and the
// input reader was rebuilt on every call, so whatever the first read had
// already buffered was silently dropped). A paste is one message.
func TestBracketedPasteIsOneMessage(t *testing.T) {
	restore := captureStdout(t)
	got, err := typed("\x1b[200~func main() {\r\tfmt.Println(1)\r}\x1b[201~\r").editLine()
	out := restore()
	if err != nil {
		t.Fatal(err)
	}
	if want := "func main() {\n\tfmt.Println(1)\n}"; got != want {
		t.Fatalf("pasted text = %q, want %q", got, want)
	}
	assertNoBareLF(t, out)
}

// Consoles without bracketed paste (conhost) deliver a paste as plain key
// input. A person cannot type Enter and further keys in the same read, so a
// newline with more input already waiting behind it is part of a paste.
func TestUnbracketedPasteIsOneMessage(t *testing.T) {
	restore := captureStdout(t)
	got, err := typed("line one\r\nline two\rline three\r").editLine()
	out := restore()
	if err != nil {
		t.Fatal(err)
	}
	if want := "line one\nline two\nline three"; got != want {
		t.Fatalf("pasted text = %q, want %q", got, want)
	}
	assertNoBareLF(t, out)
}

// A closed stdin (terminal gone, pipe ended) returned io.EOF, which the REPL
// loop treats as "reinitialise and read again" — forever, at full CPU,
// printing the same error line.
func TestEndOfInputEndsTheSession(t *testing.T) {
	if _, err := editOnce(t, ""); err != ErrExit {
		t.Fatalf("err = %v, want ErrExit", err)
	}
	line, err := editOnce(t, "last words")
	if err != nil || line != "last words" {
		t.Fatalf("got (%q, %v), want the unterminated line back", line, err)
	}
}

// inputDisplayWindow advanced its window start one rune at a time and
// re-measured the whole prefix at each step, so every keystroke cost O(n²) in
// the input length: pasting a long log froze the editor.
func TestInputDisplayWindowIsLinearInInputLength(t *testing.T) {
	buf := []rune(strings.Repeat("汉字abc", 40000)) // 200k runes
	done := make(chan struct{})
	go func() {
		inputDisplayWindow(buf, len(buf), 80)
		inputDisplayWindow(buf, len(buf)/2, 80)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("inputDisplayWindow did not finish in 5s on a 200k-rune input")
	}
}

func TestLongPasteIsAcceptedQuickly(t *testing.T) {
	text := strings.Repeat("0123456789", 5000) // 50k runes, no newline
	done := make(chan string, 1)
	restore := captureStdout(t)
	go func() {
		line, _ := typed("\x1b[200~" + text + "\x1b[201~\r").editLine()
		done <- line
	}()
	select {
	case got := <-done:
		restore()
		if got != text {
			t.Fatalf("pasted %d runes, got %d back", len(text), len(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a 50k-rune paste did not finish in 10s")
	}
}

// CJK Extension B and later planes are double width. They were measured as
// one column, so the visible window overflowed the terminal, soft-wrapped,
// and the next redraw left a ghost row behind.
func TestInputDisplayWindowFitsSupplementaryIdeographs(t *testing.T) {
	buf := []rune(strings.Repeat("\U00020021\U0002A700", 30))
	disp, _, _, _ := inputDisplayWindow(buf, len(buf), 20)
	if w := textutil.Width(string(disp)); w > 20 {
		t.Fatalf("visible window is %d columns, budget 20: %q", w, string(disp))
	}
}
