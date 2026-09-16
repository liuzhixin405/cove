package termui

import (
	"fmt"
	"sync"
	"time"
)

type Spinner struct {
	mu      sync.Mutex
	active  bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	message string
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
				s.mu.Unlock()
				PrintTransientStatus(fmt.Sprintf("  %s%s %s%s", Cyan, frame, msg, Reset))
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
				PrintTransientStatus(fmt.Sprintf("  %s%s%s %s%s%s", Cyan, frame, Reset, Dim, msg, Reset))
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
