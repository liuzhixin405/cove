package termui

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// frame 0 of each animation is deterministic: the goroutine paints before its
// first sleep, so the very first thing on stdout is index 0.
const (
	wantSpinnerFrame0 = "\x1b[0m\x1b[?25h\r\x1b[K" + "  " + Cyan + "⠋ 思考中" + Reset
	wantWalkFrame0    = "\x1b[0m\x1b[?25h\r\x1b[K" + "  " + Cyan + "> .   .   ." + Reset + " " + Dim + "思考中" + Reset
)

func TestSpinnerPaintsExactFirstFrame(t *testing.T) {
	c := captureStdout(t)
	s := NewSpinner("思考中")
	s.Start()
	t.Cleanup(s.Stop)

	c.waitFor(wantSpinnerFrame0)
	s.Stop()
	out := c.finish()

	if !strings.HasPrefix(out, wantSpinnerFrame0) {
		t.Errorf("first spinner frame\ngot prefix of: %q\nwant:          %q", out, wantSpinnerFrame0)
	}
	// Stop must erase the status line rather than leaving the last frame behind.
	if want := "\x1b[0m\x1b[?25h\r\x1b[K"; !strings.HasSuffix(out, want) {
		t.Errorf("spinner did not clear the line on Stop; output ends %q", tail(out))
	}
}

func TestSpinnerCyclesThroughItsFrames(t *testing.T) {
	c := captureStdout(t)
	s := NewSpinner("x")
	s.Start()
	t.Cleanup(s.Stop)

	// Frames advance, they do not repaint index 0 forever.
	c.waitFor(Cyan + "⠋ x")
	c.waitFor(Cyan + "⠙ x")
	c.waitFor(Cyan + "⠹ x")
	s.Stop()
	c.finish()
}

func TestSpinnerSetMessageChangesWhatIsPainted(t *testing.T) {
	c := captureStdout(t)
	s := NewSpinner("第一条消息")
	s.Start()
	t.Cleanup(s.Stop)

	c.waitFor("第一条消息")
	s.SetMessage("第二条消息")
	c.waitFor("第二条消息")

	s.Stop()
	c.finish()
}

// TestSpinnerSetMessageIsRaceFree is the regression test for a real data race:
// the animation goroutine read s.message without holding s.mu while SetMessage
// wrote it under the mutex. Run with -race.
func TestSpinnerSetMessageIsRaceFree(t *testing.T) {
	c := captureStdout(t)
	s := NewSpinner("start")
	s.Start()
	t.Cleanup(s.Stop)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			s.SetMessage(strings.Repeat("消息", i%7+1))
		}
	}()
	wg.Wait()

	s.Stop()
	c.finish()
}

func TestSpinnerCanBeStartedAgainAfterStop(t *testing.T) {
	c := captureStdout(t)
	s := NewSpinner("第一轮")
	s.Start()
	t.Cleanup(s.Stop)
	c.waitFor("第一轮")
	s.Stop()

	// A restarted spinner must actually animate again, not sit silently dead.
	s.SetMessage("第二轮")
	s.Start()
	c.waitFor("第二轮")
	s.Stop()
	c.finish()
}

func TestSpinnerDoubleStartIsANoOpAndStopStillReturns(t *testing.T) {
	c := captureStdout(t)
	s := NewSpinner("dup")
	s.Start()
	t.Cleanup(s.Stop)
	c.waitFor("dup")

	s.Start() // must not launch a second goroutine sharing the same channels
	s.Stop()
	s.Stop() // idempotent

	// Still usable afterwards.
	s.SetMessage("after")
	s.Start()
	c.waitFor("after")
	s.Stop()
	c.finish()
}

func TestSpinnerStopWithoutStartIsANoOp(t *testing.T) {
	c := captureStdout(t)
	NewSpinner("never started").Stop()
	if out := c.finish(); out != "" {
		t.Errorf("Stop() on an unstarted spinner wrote %q, want nothing", out)
	}
}

func TestWalkingIndicatorPaintsExactFirstFrame(t *testing.T) {
	c := captureStdout(t)
	w := NewWalkingIndicator("思考中")
	w.Start()
	t.Cleanup(w.Stop)

	c.waitFor(wantWalkFrame0)
	w.Stop()
	out := c.finish()

	if !strings.HasPrefix(out, wantWalkFrame0) {
		t.Errorf("first walking frame\ngot prefix of: %q\nwant:          %q", out, wantWalkFrame0)
	}
	if want := "\x1b[0m\x1b[?25h\r\x1b[K"; !strings.HasSuffix(out, want) {
		t.Errorf("walker did not clear the line on Stop; output ends %q", tail(out))
	}
}

func TestWalkingIndicatorCyclesThroughItsFrames(t *testing.T) {
	c := captureStdout(t)
	w := NewWalkingIndicator("x")
	w.Start()
	t.Cleanup(w.Stop)

	c.waitFor(Cyan + "> .   .   ." + Reset)
	c.waitFor(Cyan + ">  .   .   " + Reset)
	c.waitFor(Cyan + ">   .   .  " + Reset)
	w.Stop()
	c.finish()
}

func TestWalkingIndicatorSetMessageChangesWhatIsPainted(t *testing.T) {
	c := captureStdout(t)
	w := NewWalkingIndicator("思考中")
	w.Start()
	t.Cleanup(w.Stop)

	c.waitFor("思考中")
	w.SetMessage("正在写文件")
	c.waitFor("正在写文件")

	w.Stop()
	c.finish()
}

// TestWalkingIndicatorCanBeStartedAgainAfterStop is the regression test for a
// real crash: stopCh was allocated once in the constructor instead of in
// Start(), so after one Start/Stop cycle the channel stayed closed. The second
// Start()'s goroutine saw it closed and exited immediately (a silently dead
// indicator), and the second Stop() panicked with "close of closed channel".
func TestWalkingIndicatorCanBeStartedAgainAfterStop(t *testing.T) {
	c := captureStdout(t)
	w := NewWalkingIndicator("第一轮")
	w.Start()
	t.Cleanup(w.Stop)
	c.waitFor("第一轮")
	w.Stop()

	w.SetMessage("第二轮")
	w.Start() // must animate again
	c.waitFor("第二轮")
	w.Stop() // must not panic
	c.finish()
}

func TestWalkingIndicatorSurvivesManyStartStopCycles(t *testing.T) {
	c := captureStdout(t)
	w := NewWalkingIndicator("cycle-0")
	t.Cleanup(w.Stop)
	for i := 0; i < 5; i++ {
		// A distinct message per round, so waiting for it proves *this* round
		// painted rather than matching output left over from a previous one.
		msg := "cycle-" + string(rune('A'+i))
		w.SetMessage(msg)
		w.Start()
		c.waitFor(msg)
		w.Stop()
	}
	c.finish()
}

func TestWalkingIndicatorDoubleStartIsANoOpAndStopStillReturns(t *testing.T) {
	c := captureStdout(t)
	w := NewWalkingIndicator("dup")
	w.Start()
	t.Cleanup(w.Stop)
	c.waitFor("dup")

	w.Start()
	w.Stop()
	w.Stop()

	w.SetMessage("after")
	w.Start()
	c.waitFor("after")
	w.Stop()
	c.finish()
}

func TestWalkingIndicatorStopWithoutStartIsANoOp(t *testing.T) {
	c := captureStdout(t)
	NewWalkingIndicator("never started").Stop()
	if out := c.finish(); out != "" {
		t.Errorf("Stop() on an unstarted walker wrote %q, want nothing", out)
	}
}

// TestWalkingIndicatorSetMessageIsRaceFree hammers SetMessage while the
// animation goroutine paints. Run with -race.
func TestWalkingIndicatorSetMessageIsRaceFree(t *testing.T) {
	c := captureStdout(t)
	w := NewWalkingIndicator("start")
	w.Start()
	t.Cleanup(w.Stop)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			w.SetMessage(strings.Repeat("步", i%7+1))
		}
	}()
	wg.Wait()

	w.Stop()
	c.finish()
}

// TestIndicatorStopReturnsPromptly asserts Stop joins its goroutine instead of
// hanging: the frame sleep is 80-120ms, so a couple of seconds is generous.
func TestIndicatorStopReturnsPromptly(t *testing.T) {
	c := captureStdout(t)
	s := NewSpinner("spin-msg")
	w := NewWalkingIndicator("walk-msg")
	s.Start()
	t.Cleanup(s.Stop)
	c.waitFor("spin-msg")
	w.Start()
	t.Cleanup(w.Stop)
	c.waitFor("walk-msg")

	done := make(chan struct{})
	go func() {
		s.Stop()
		w.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within 5s; the animation goroutine was not joined")
	}
	c.finish()
}

func tail(s string) string {
	if len(s) > 60 {
		return s[len(s)-60:]
	}
	return s
}
