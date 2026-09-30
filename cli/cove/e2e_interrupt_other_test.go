//go:build !windows

package main

import (
	"os"
	"syscall"
)

// Interrupt is Ctrl+C while a task runs. With the terminal in cooked mode
// (the plain reader, a running task) the key arrives as SIGINT, so this
// sends the process a real SIGINT and the REPL's handler cancels the task.
// Only call it while the REPL runs: its handler is what keeps SIGINT from
// ending the test binary.
func (s *e2eSession) Interrupt() {
	s.t.Helper()
	select {
	case <-s.done:
		s.t.Fatal("Interrupt after the REPL exited would kill the test binary")
	default:
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		s.t.Fatalf("SIGINT: %v", err)
	}
}

// interruptIsSignal reports whether Interrupt delivers a real Ctrl+C.
const interruptIsSignal = true
