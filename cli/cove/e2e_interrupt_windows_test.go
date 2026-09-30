//go:build windows

package main

// Interrupt is Ctrl+C while a task runs. Windows delivers Ctrl+C as a console
// control event to every process on the console (go test and the shell
// included), so a test cannot send one to itself alone; it types /stop,
// which cancels the task through the same two calls the SIGINT handler makes
// (denyPendingPermissionPrompt, then CancelRunning). The raw-mode 0x03 key
// path is covered by internal/repl.
func (s *e2eSession) Interrupt() {
	s.t.Helper()
	s.Type("/stop")
}

// interruptIsSignal reports whether Interrupt delivers a real Ctrl+C.
const interruptIsSignal = false
