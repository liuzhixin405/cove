package termui

import (
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// terminalWidth reports the width of the terminal on stdout, or 0 when it
// cannot be determined (a pipe, a test). Replaced in tests.
var terminalWidth = func() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0
	}
	return w
}

// fitStatusLine clips a transient status frame to one row of the terminal.
//
// Frames used to be printed whole. The spinner's "思考中… 已推理 N 字" plus
// its elapsed/context/cost suffix is ~60 columns, so in a narrower pane every
// 80 ms frame wrapped onto a second row; the next frame's "\r\x1b[K" returns
// to the start of that second row only, so each frame left the previous first
// row behind. The budget is width-1 because writing the last column makes
// some terminals (the Windows console) wrap early. Width is measured the way
// textutil.Width measures it, CJK and East Asian Ambiguous glyphs as two
// columns. Reset is re-appended since the cut may drop the frame's own.
func fitStatusLine(line string) string {
	w := terminalWidth()
	if w <= 1 || textutil.Width(line) <= w-1 {
		return line
	}
	return textutil.TruncateWidth(line, w-1, "…") + Reset
}

type Spinner struct {
	mu      sync.Mutex
	active  bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	message string
	// suffix, when set, is evaluated on every frame and shown dim after the
	// message (elapsed time, context use, cost).
	suffix func() string
}

// SetSuffix sets the live status shown after the message.
func (s *Spinner) SetSuffix(f func() string) {
	s.mu.Lock()
	s.suffix = f
	s.mu.Unlock()
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func NewSpinner(message string) *Spinner {
	return &Spinner{message: message}
}

func (s *Spinner) Start() {
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return
	}
	s.active = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	stopCh := s.stopCh
	doneCh := s.doneCh
	s.mu.Unlock()

	go func() {
		defer close(doneCh)
		i := 0
		lastSuffix := ""
		for {
			select {
			case <-stopCh:
				PrintTransientStatus("")
				return
			default:
				frame := spinnerFrames[i%len(spinnerFrames)]
				// message must be read under the lock: SetMessage writes it
				// from the caller's goroutine, so the bare s.message read here
				// was a data race on a string — which can tear into a
				// mismatched pointer/length pair, not just show a stale value.
				// (WalkingIndicator below already did this correctly.)
				s.mu.Lock()
				msg := s.message
				suffix := s.suffix
				s.mu.Unlock()
				line := fmt.Sprintf("  %s%s %s%s", Cyan, frame, msg, Reset)
				// The suffix is recomputed every ~0.5s, not every frame.
				if suffix != nil {
					if i%6 == 0 || lastSuffix == "" {
						lastSuffix = suffix()
					}
					if lastSuffix != "" {
						line += "  " + Dim + lastSuffix + Reset
					}
				}
				PrintTransientStatus(fitStatusLine(line))
				i++
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()
}

func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	stopCh := s.stopCh
	doneCh := s.doneCh
	s.mu.Unlock()

	close(stopCh)
	<-doneCh
}

func (s *Spinner) SetMessage(msg string) {
	s.mu.Lock()
	s.message = msg
	s.mu.Unlock()
}

type WalkingIndicator struct {
	mu      sync.Mutex
	active  bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	message string
}

var walkFrames = []string{
	"> .   .   .",
	">  .   .   ",
	">   .   .  ",
	">    .   . ",
	"> .   .   .",
	">  .   .   ",
}

func NewWalkingIndicator(message string) *WalkingIndicator {
	return &WalkingIndicator{message: message, stopCh: make(chan struct{})}
}

func (w *WalkingIndicator) Start() {
	w.mu.Lock()
	if w.active {
		w.mu.Unlock()
		return
	}
	w.active = true
	w.doneCh = make(chan struct{})
	// stopCh is recreated on every Start. It is closed by Stop, and a closed
	// channel stays closed — so reusing the one from the constructor meant the
	// goroutine of a second Start saw an immediately-ready stopCh and exited
	// at once (the indicator was silently dead after the first Stop), and the
	// second Stop then panicked with "close of closed channel".
	w.stopCh = make(chan struct{})
	stopCh := w.stopCh
	doneCh := w.doneCh
	w.mu.Unlock()

	go func() {
		defer close(doneCh)
		i := 0
		for {
			select {
			case <-stopCh:
				PrintTransientStatus("")
				return
			default:
				frame := walkFrames[i%len(walkFrames)]
				w.mu.Lock()
				msg := w.message
				w.mu.Unlock()
				PrintTransientStatus(fitStatusLine(fmt.Sprintf("  %s%s%s %s%s%s", Cyan, frame, Reset, Dim, msg, Reset)))
				i++
				time.Sleep(120 * time.Millisecond)
			}
		}
	}()
}

func (w *WalkingIndicator) Stop() {
	w.mu.Lock()
	if !w.active {
		w.mu.Unlock()
		return
	}
	w.active = false
	close(w.stopCh)
	doneCh := w.doneCh
	w.mu.Unlock()
	<-doneCh
}

func (w *WalkingIndicator) SetMessage(msg string) {
	w.mu.Lock()
	w.message = msg
	w.mu.Unlock()
}
