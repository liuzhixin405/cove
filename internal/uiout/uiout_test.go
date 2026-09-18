package uiout

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/render"
)

func TestWriterSinkRendersBlocksCollapsed(t *testing.T) {
	var buf bytes.Buffer
	s := NewWriter(&buf, 80, render.Styles{})

	s.Block(render.ToolBlock("a1", "bash", "go build ./...", "", "ok\nmore output", false, 0))
	out := buf.String()

	if !strings.Contains(out, "bash") || !strings.Contains(out, "go build") {
		t.Errorf("block was not rendered: %q", out)
	}
	// Collapsed, so the hidden body must NOT appear.
	if strings.Contains(out, "more output") {
		t.Errorf("writer sink expanded a collapsed block: %q", out)
	}
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; n != 2 {
		t.Errorf("collapsed tool wrote %d lines, want 2: %q", n, out)
	}
}

func TestWriterSinkDropsActivity(t *testing.T) {
	var buf bytes.Buffer
	s := NewWriter(&buf, 80, render.Styles{})

	s.Activity("running go test…")
	if buf.Len() != 0 {
		t.Errorf("transient activity leaked into a piped stream: %q", buf.String())
	}

	// Lines still go through.
	s.Line("real output")
	if !strings.Contains(buf.String(), "real output") {
		t.Errorf("Line was dropped: %q", buf.String())
	}
}

func TestWriterSinkDefaultsWidth(t *testing.T) {
	var buf bytes.Buffer
	// A zero or negative width arrives during startup before the terminal size
	// is known; it must not produce a degenerate render.
	for _, w := range []int{0, -1} {
		buf.Reset()
		NewWriter(&buf, w, render.Styles{}).Block(render.UserBlock("hello"))
		if strings.TrimSpace(buf.String()) == "" {
			t.Errorf("width=%d produced no output", w)
		}
	}
}

func TestFuncSinkRoutesEachKind(t *testing.T) {
	var gotBlock render.Block
	var gotLine, gotActivity string

	s := NewFuncs(Funcs{
		OnBlock:    func(b render.Block) { gotBlock = b },
		OnLine:     func(l string) { gotLine = l },
		OnActivity: func(a string) { gotActivity = a },
	})

	s.Block(render.UserBlock("hi"))
	s.Line("a line")
	s.Activity("busy")

	if gotBlock.Header != "hi" {
		t.Errorf("OnBlock got %+v", gotBlock)
	}
	if gotLine != "a line" {
		t.Errorf("OnLine got %q", gotLine)
	}
	if gotActivity != "busy" {
		t.Errorf("OnActivity got %q", gotActivity)
	}
}

// TestFuncSinkToleratesNilCallbacks covers the partial-wiring case: a front end
// that only handles lines must not panic when a block arrives.
func TestFuncSinkToleratesNilCallbacks(t *testing.T) {
	s := NewFuncs(Funcs{})
	s.Block(render.UserBlock("x"))
	s.Line("y")
	s.Activity("z")
}

// TestDiscardIsSafeDefault pins the contract that an unwired component stays
// silent rather than writing to the terminal behind the renderer's back — the
// exact failure this package exists to prevent.
func TestDiscardIsSafeDefault(t *testing.T) {
	Discard.Block(render.ToolBlock("a1", "bash", "x", "", "y", false, 0))
	Discard.Line("nope")
	Discard.Activity("nope")
}

func TestCaptureRecordsEverything(t *testing.T) {
	c := NewCapture()
	c.Block(render.UserBlock("q"))
	c.Block(render.AnswerBlock("a"))
	c.Line("l1")
	c.Activity("busy")

	if got := len(c.Blocks()); got != 2 {
		t.Errorf("Blocks() = %d, want 2", got)
	}
	if got := c.Lines(); len(got) != 1 || got[0] != "l1" {
		t.Errorf("Lines() = %v", got)
	}
	if got := c.Activities(); len(got) != 1 || got[0] != "busy" {
		t.Errorf("Activities() = %v", got)
	}

	// The accessors must hand back copies, or a caller mutating the slice
	// would corrupt the capture.
	blocks := c.Blocks()
	blocks[0].Header = "mutated"
	if c.Blocks()[0].Header == "mutated" {
		t.Error("Blocks() leaked the internal slice")
	}

	c.Reset()
	if len(c.Blocks())+len(c.Lines())+len(c.Activities()) != 0 {
		t.Error("Reset did not clear the capture")
	}
}

// TestSinksAreConcurrencySafe is the property the doc comment promises: the
// engine, its turn-end pipeline, async hooks and the MCP pool all write from
// their own goroutines. Run with -race.
func TestSinksAreConcurrencySafe(t *testing.T) {
	sinks := map[string]Sink{
		"writer":  NewWriter(&bytes.Buffer{}, 80, render.Styles{}),
		"capture": NewCapture(),
		"discard": Discard,
		"funcs":   NewFuncs(Funcs{OnBlock: func(render.Block) {}, OnLine: func(string) {}, OnActivity: func(string) {}}),
	}

	for name, s := range sinks {
		t.Run(name, func(t *testing.T) {
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for j := 0; j < 50; j++ {
						s.Block(render.ToolBlock("a1", "bash", "cmd", "", "out", false, 0))
						s.Line("line")
						s.Activity("busy")
					}
				}(i)
			}
			wg.Wait()
		})
	}
}

// TestCaptureCountIsExactUnderConcurrency proves the capture does not lose
// writes, which would make any test built on it quietly unreliable.
func TestCaptureCountIsExactUnderConcurrency(t *testing.T) {
	c := NewCapture()
	const goroutines, each = 8, 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				c.Line("x")
			}
		}()
	}
	wg.Wait()

	if got, want := len(c.Lines()), goroutines*each; got != want {
		t.Fatalf("captured %d lines, want %d", got, want)
	}
}
