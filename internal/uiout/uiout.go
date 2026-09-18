// Package uiout is the single channel through which cove writes to the user.
//
// Why it has to exist before the live-region UI can work:
//
// Bubble Tea's inline renderer keeps the input box pinned at the bottom by
// bookkeeping — it remembers how many rows it drew and where the cursor ended
// up, then moves back up to repaint. Any write that bypasses the program
// invalidates that bookkeeping, and the input box drifts or leaves ghosts
// behind. Today cove writes to the terminal from ~330 places:
// fmt.Print*/os.Stdout/os.Stderr (189), termui.PrintSafe and friends (140),
// plus three that are easy to miss — engine.engineOutput's stderr fallback,
// internal/log's default stderr writer, and MCP child processes that inherit
// the terminal directly (which no Go-side change can fix; that one needs a
// pipe).
//
// So every one of those becomes a call on a Sink, and the front end decides
// what a Sink does: print straight to stdout (headless, where there is no
// program to corrupt) or hand the text to the Bubble Tea program.
//
// The interface is deliberately small. It is not a logging façade and carries
// no levels or formatting policy — those belong to internal/log and
// internal/render respectively.
package uiout

import (
	"fmt"
	"io"
	"sync"

	"github.com/liuzhixin405/cove/internal/render"
)

// Sink receives everything destined for the user's terminal.
//
// Implementations must be safe for concurrent use: the engine, its background
// turn-end pipeline, async hooks and the MCP pool all write from their own
// goroutines.
type Sink interface {
	// Block appends one structured conversation event. This is the normal
	// path: the front end renders it collapsed and decides whether it belongs
	// in the scrollback or the live region.
	Block(b render.Block)

	// Line appends one already-rendered line of text. It exists for output
	// that is not part of the conversation model — command results, banners,
	// diagnostics. The text must already be styled and wrapped; Sink does not
	// reformat it.
	Line(s string)

	// Activity replaces the transient status text ("running go test…").
	// It is NOT history: a front end with a live region overwrites it on every
	// update, and a plain-stdout front end may drop it entirely. Passing ""
	// clears it.
	Activity(s string)
}

// ---------------------------------------------------------------------------
// Writer sink — headless / piped output
// ---------------------------------------------------------------------------

// writerSink writes plain text to an io.Writer. Used by the headless front end
// and by tests. There is no program to corrupt here, so it writes directly.
//
// Activity is discarded: a transient spinner in a piped or redirected stream is
// noise that ends up in the user's log file.
type writerSink struct {
	mu    sync.Mutex
	w     io.Writer
	width int
	st    render.Styles
}

// NewWriter returns a Sink that writes to w, rendering blocks collapsed at the
// given width.
func NewWriter(w io.Writer, width int, st render.Styles) Sink {
	if width <= 0 {
		width = 80
	}
	return &writerSink{w: w, width: width, st: st}
}

func (s *writerSink) Block(b render.Block) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.w, render.Collapsed(b, s.width, s.st))
}

func (s *writerSink) Line(str string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.w, str)
}

func (s *writerSink) Activity(string) {}

// ---------------------------------------------------------------------------
// Func sink — front-end adapter
// ---------------------------------------------------------------------------

// Funcs adapts a front end to Sink without that front end having to implement
// the interface itself. A nil field is a no-op, so a caller can wire up only
// what it handles.
type Funcs struct {
	OnBlock    func(render.Block)
	OnLine     func(string)
	OnActivity func(string)
}

// NewFuncs returns a Sink backed by the given callbacks.
//
// The callbacks are invoked on the caller's goroutine, so a front end that is
// not itself goroutine-safe must hand off (a Bubble Tea front end does this
// naturally: Program.Send is safe from any goroutine).
func NewFuncs(f Funcs) Sink { return &funcSink{f: f} }

type funcSink struct{ f Funcs }

func (s *funcSink) Block(b render.Block) {
	if s.f.OnBlock != nil {
		s.f.OnBlock(b)
	}
}

func (s *funcSink) Line(str string) {
	if s.f.OnLine != nil {
		s.f.OnLine(str)
	}
}

func (s *funcSink) Activity(str string) {
	if s.f.OnActivity != nil {
		s.f.OnActivity(str)
	}
}

// ---------------------------------------------------------------------------
// Discard
// ---------------------------------------------------------------------------

// Discard drops everything. It is the safe default for a component whose sink
// has not been wired yet — silence is strictly better than a stray write that
// corrupts the live region, which is exactly the failure this package exists
// to prevent.
var Discard Sink = discardSink{}

type discardSink struct{}

func (discardSink) Block(render.Block) {}
func (discardSink) Line(string)        {}
func (discardSink) Activity(string)    {}

// ---------------------------------------------------------------------------
// Capture — tests
// ---------------------------------------------------------------------------

// Capture records everything it receives. Tests assert against it instead of
// scraping stdout.
type Capture struct {
	mu         sync.Mutex
	blocks     []render.Block
	lines      []string
	activities []string
}

// NewCapture returns an empty Capture.
func NewCapture() *Capture { return &Capture{} }

func (c *Capture) Block(b render.Block) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks = append(c.blocks, b)
}

func (c *Capture) Line(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, s)
}

func (c *Capture) Activity(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activities = append(c.activities, s)
}

// Blocks returns a copy of the recorded blocks.
func (c *Capture) Blocks() []render.Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]render.Block(nil), c.blocks...)
}

// Lines returns a copy of the recorded lines.
func (c *Capture) Lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// Activities returns a copy of the recorded activity updates.
func (c *Capture) Activities() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.activities...)
}

// Reset clears everything recorded so far.
func (c *Capture) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks, c.lines, c.activities = nil, nil, nil
}
