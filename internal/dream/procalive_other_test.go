//go:build !windows

package dream

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// kill(pid, 0) fails with EPERM for a process that exists but belongs to
// another user: it is alive, and its lock must stay held. ESRCH is the only
// "no such process" answer.
func TestAliveAfterSignalError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"signalled", nil, true},
		{"EPERM is a live process of another user", syscall.EPERM, true},
		{"wrapped EPERM", fmt.Errorf("kill: %w", syscall.EPERM), true},
		{"wrapped EPERM in a SyscallError", os.NewSyscallError("kill", syscall.EPERM), true},
		{"ESRCH is dead", syscall.ESRCH, false},
		{"os.ErrProcessDone is dead", os.ErrProcessDone, false},
		{"unknown error gives no verdict", errors.New("boom"), true},
	} {
		if got := aliveAfterSignalError(tc.err); got != tc.want {
			t.Errorf("%s: aliveAfterSignalError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// PID 1 (init/launchd) is alive on every Unix and, for an unprivileged
// tester, signalling it fails with EPERM.
func TestIsProcessRunningInit(t *testing.T) {
	if !isProcessRunning(1) {
		t.Fatal("isProcessRunning(1) = false; PID 1 is alive (EPERM must count as alive)")
	}
}
