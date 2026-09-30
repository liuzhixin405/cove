//go:build windows

package dream

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

// OpenProcess fails with ERROR_ACCESS_DENIED for a process that exists but
// runs at a higher integrity level or as another user (an elevated cove, a
// worker started from a service). Every error used to mean "dead", so a
// live holder's lock was judged stale and taken over, and recoverDeadWorker
// rolled a live worker's lock back under it. Only "no such process"
// (ERROR_INVALID_PARAMETER) means dead; an error that gives no verdict keeps
// the lock held, since holderStaleMs reclaims it anyway.
func TestAliveAfterOpenProcessError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no such process", windows.ERROR_INVALID_PARAMETER, false},
		{"access denied is a live process", windows.ERROR_ACCESS_DENIED, true},
		{"wrapped access denied", fmt.Errorf("OpenProcess: %w", windows.ERROR_ACCESS_DENIED), true},
		{"wrapped invalid parameter", fmt.Errorf("OpenProcess: %w", windows.ERROR_INVALID_PARAMETER), false},
		{"unknown errno gives no verdict", windows.ERROR_NOT_ENOUGH_MEMORY, true},
		{"non-errno error gives no verdict", errors.New("boom"), true},
	} {
		if got := aliveAfterOpenError(tc.err); got != tc.want {
			t.Errorf("%s: aliveAfterOpenError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// A handle that opened belongs to a process that exists, but it may have
// exited already (a parent still holds a handle): only STILL_ACTIVE is alive.
func TestAliveFromExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		code uint32
		err  error
		want bool
	}{
		{"still active", stillActive, nil, true},
		{"exited 0", 0, nil, false},
		{"exited 1", 1, nil, false},
		{"exit code unreadable: handle opened, so it exists", 0, windows.ERROR_ACCESS_DENIED, true},
	} {
		if got := aliveFromExitCode(tc.code, tc.err); got != tc.want {
			t.Errorf("%s: aliveFromExitCode(%d, %v) = %v, want %v", tc.name, tc.code, tc.err, got, tc.want)
		}
	}
}

// The System process (PID 4) is alive on every Windows and cannot be opened
// with PROCESS_QUERY_INFORMATION from a normal user: it is the real-world case
// the old FindProcess-based check got wrong.
func TestIsProcessRunningSystemProcess(t *testing.T) {
	if !isProcessRunning(4) {
		t.Fatal("isProcessRunning(4) = false; the System process is alive but cannot be opened for query")
	}
}
