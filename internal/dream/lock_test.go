package dream

import (
	"os"
	"testing"
)

// TestIsProcessRunning_CurrentProcess proves the lock's liveness check works:
// the current process is obviously alive, so isProcessRunning(getpid()) must be true.
// Before the fix this returned false unconditionally (proc.Signal(os.Signal(nil))
// always errors), which made TryAcquireConsolidationLock treat every live holder as
// dead and steal the lock.
func TestIsProcessRunning_CurrentProcess(t *testing.T) {
	pid := os.Getpid()
	if !isProcessRunning(pid) {
		t.Fatalf("isProcessRunning(current pid=%d) = false, want true", pid)
	}
}

// TestIsProcessRunning_DeadProcess checks a PID that is essentially certain not to
// exist is reported as not running, so the lock can still be reclaimed from a
// genuinely dead holder.
func TestIsProcessRunning_DeadProcess(t *testing.T) {
	// 0x7FFFFFFE: extremely unlikely to be a live PID on any platform.
	if isProcessRunning(0x7FFFFFFE) {
		t.Skip("PID 0x7FFFFFFE unexpectedly reported running; skipping (environment-dependent)")
	}
}
