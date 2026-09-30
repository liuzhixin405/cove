//go:build !windows

package dream

import (
	"errors"
	"os"
	"syscall"
)

// isProcessRunning reports whether a process with the given PID is alive.
//
// The previous implementation was `proc.Signal(os.Signal(nil))`, which can
// only ever fail: os.Signal is an interface, so a nil interface value fails
// the type assertion inside Signal and an error is returned unconditionally.
// That made this function return false for EVERY pid, so a lock held by a
// live process was always judged stale and reclaimed, and the consolidation
// lock provided no mutual exclusion at all. FindProcess always succeeds on
// Unix regardless of liveness; the probe is kill(pid, 0).
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return aliveAfterSignalError(proc.Signal(syscall.Signal(0)))
}

// aliveAfterSignalError classifies kill(pid, 0)'s result. A nil error means
// the process exists and is signalable; EPERM means it exists but belongs to
// another user (alive: its lock is valid). ESRCH (or os.ErrProcessDone, which
// os.Process reports for a process it knows has gone) is the only "no such
// process". Any other error gives no verdict and the lock stays held; the
// stale age reclaims it.
func aliveAfterSignalError(err error) bool {
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return false
	}
	return true
}
