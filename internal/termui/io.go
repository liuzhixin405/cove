package termui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

var consoleMu sync.Mutex

// out is where every function in this package writes.
//
// It is redirectable because this file is the single chokepoint for ~140
// PrintSafe/PrintAbove/StreamPrint call sites across the codebase. A front end
// that manages its own frame — anything with a pinned input box — keeps the
// prompt in place by tracking how many rows it drew; a direct fmt.Print from
// here lands outside that bookkeeping and leaves the prompt drifting or
// ghosted. Redirecting once here neutralizes all 140 without touching them.
//
// nil means "whatever os.Stdout is at the moment of the write" — it is NOT
// eagerly resolved to the current os.Stdout.
//
// That distinction matters: capturing os.Stdout into this variable at init
// would break every caller that redirects output by reassigning os.Stdout
// (the os.Pipe + os.Stdout = w idiom this package's own tests use), because
// writes would keep going to the original handle. Resolving late keeps the
// pre-existing behaviour exactly while still allowing an explicit redirect.
var out io.Writer

// SetWriter redirects this package's output. Pass io.Discard to silence it, or
// a front end's own writer to route it. Passing nil restores the default of
// following os.Stdout.
//
// Call it before the front end starts drawing; it is safe to call at any time,
// but a write already in flight goes to the previous destination.
func SetWriter(w io.Writer) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	out = w
}

// Console is the protocol a line editor implements so this package can print
// above an editable input line instead of on top of it.
//
// It exists because there were two copies of this protocol — one here, one in
// internal/repl — and only the second one knew about the input line. So the
// ~140 call sites that use this package printed over the prompt, while the
// editor's own writes did the right thing. One protocol, one implementation:
// the editor registers itself and this package delegates.
//
// The methods must be safe to call from any goroutine; the editor serialises
// them on its own lock, which is why this package does not hold consoleMu
// across the delegation.
type Console interface {
	// PrintAbove writes a block of output above the input line.
	PrintAbove(s string)
	// StreamPrint writes a partial chunk that may not end in a newline.
	StreamPrint(s string)
	// Transient replaces a self-overwriting status line (a spinner frame).
	Transient(s string)
	// BeginOutput and EndOutput bracket a turn: between them there is no
	// editable input line on screen, so output can stream without erasing and
	// redrawing anything.
	BeginOutput()
	EndOutput()
}

var console Console

// SetConsole installs the line editor that owns the terminal. Passing nil
// restores plain writing, which is correct for the headless front end where
// there is no input line to protect.
func SetConsole(c Console) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	console = c
}

// activeConsole returns the installed editor, if any.
func activeConsole() Console {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	return console
}

// Writer returns the current destination, resolving the default.
func Writer() io.Writer {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	return dest()
}

// dest resolves where to write. Callers must hold consoleMu.
func dest() io.Writer {
	if out != nil {
		return out
	}
	return os.Stdout
}

// write is the one place this package touches its destination.
func write(s string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Fprint(dest(), s)
}

func normalizeOutputNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func PrintSafe(format string, args ...any) {
	PrintAbove(fmt.Sprintf(format, args...))
}

func PrintAbove(s string) {
	if c := activeConsole(); c != nil {
		c.PrintAbove(s)
		return
	}
	write(normalizeOutputNewlines(s))
}

func StreamPrint(s string) {
	if c := activeConsole(); c != nil {
		c.StreamPrint(s)
		return
	}
	write(normalizeOutputNewlines(s))
}

// PrintTransientStatus overwrites the current line with s.
//
// The leading escapes reset styling, show the cursor, return to column 0 and
// erase to end of line. They are exactly the kind of cursor-moving sequence a
// frame-managing front end must never receive, which is why such a front end
// redirects this package rather than letting it through.
func PrintTransientStatus(s string) {
	if c := activeConsole(); c != nil {
		c.Transient(s)
		return
	}
	write("\x1b[0m\x1b[?25h\r\x1b[K" + s)
}

func BeginOutput() {
	if c := activeConsole(); c != nil {
		c.BeginOutput()
		return
	}
	write("\n")
}

func EndOutput() {
	if c := activeConsole(); c != nil {
		c.EndOutput()
		return
	}
	write("\r\n")
}
