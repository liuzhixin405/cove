package termui

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stdoutCapture redirects os.Stdout (which every printer in io.go writes
// through) into a pipe and accumulates the bytes so a test can assert on the
// exact control sequences emitted.
//
// Ordering matters for -race: os.Stdout is swapped before any indicator
// goroutine is started (the `go` statement gives the happens-before edge) and
// restored only after that goroutine has been joined by Stop(). Tests must not
// call t.Parallel().
type stdoutCapture struct {
	t    *testing.T
	old  *os.File
	r, w *os.File
	done chan struct{}

	mu      sync.Mutex
	buf     bytes.Buffer
	restore sync.Once
}

func captureStdout(t *testing.T) *stdoutCapture {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	c := &stdoutCapture{t: t, old: os.Stdout, r: r, w: w, done: make(chan struct{})}
	os.Stdout = w

	go func() {
		defer close(c.done)
		chunk := make([]byte, 4096)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				c.mu.Lock()
				c.buf.Write(chunk[:n])
				c.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Registered first so it runs last (Cleanup is LIFO): any indicator Stop
	// a test registers afterwards joins its goroutine before os.Stdout moves.
	t.Cleanup(func() { c.restore.Do(func() { os.Stdout = c.old }) })
	return c
}

// text returns everything captured so far.
func (c *stdoutCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// waitFor blocks until want appears in the captured output. Failing here means
// the writer never produced it, not that a sleep was too short.
func (c *stdoutCapture) waitFor(want string) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.text(), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for %q on stdout; captured %q", want, c.text())
}

// finish restores os.Stdout and returns the complete captured output. Call it
// only after every goroutine writing to stdout has been joined.
func (c *stdoutCapture) finish() string {
	c.t.Helper()
	c.restore.Do(func() { os.Stdout = c.old })
	c.w.Close()
	<-c.done
	c.r.Close()
	return c.text()
}
