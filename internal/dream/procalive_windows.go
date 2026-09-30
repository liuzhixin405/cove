//go:build windows

package dream

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE, GetExitCodeProcess's code for a process that
// has not exited.
const stillActive = 259

// isProcessRunning reports whether a process with the given PID is alive.
//
// It used to be os.FindProcess, which opens the process with
// PROCESS_QUERY_INFORMATION|SYNCHRONIZE and fails with ERROR_ACCESS_DENIED
// for a live process at a higher integrity level or of another user (an
// elevated cove, a worker started by a service); every error was read as
// "dead". A live holder's consolidation lock was then judged stale and taken
// over (two consolidations on the same files), and recoverDeadWorker rolled
// a live worker's lock back under it. The process is now opened with the
// least right (PROCESS_QUERY_LIMITED_INFORMATION, granted across integrity
// levels), an open failure is classified by its error, and an opened handle
// is asked whether the process has exited (a handle someone still holds
// keeps an exited process openable).
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // PIDs are small positive OS values
	if err != nil {
		return aliveAfterOpenError(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	err = windows.GetExitCodeProcess(h, &code)
	return aliveFromExitCode(code, err)
}

// aliveAfterOpenError classifies an OpenProcess failure. Only "no such
// process" (ERROR_INVALID_PARAMETER) means dead. ERROR_ACCESS_DENIED means
// the process exists and we may not open it: alive. Any other error gives
// no verdict, and a lock is then left held: holderStaleMs reclaims it
// anyway, while taking it over from a live holder is what this guards.
func aliveAfterOpenError(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case windows.ERROR_INVALID_PARAMETER:
			return false
		case windows.ERROR_ACCESS_DENIED:
			return true
		}
	}
	return true
}

// aliveFromExitCode reads GetExitCodeProcess's answer for an opened handle:
// the process exists, and is alive unless it reports an exit code. A failed
// query still means the handle opened, so the process exists.
func aliveFromExitCode(code uint32, err error) bool {
	if err != nil {
		return true
	}
	return code == stillActive
}
