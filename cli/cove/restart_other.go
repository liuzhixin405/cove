//go:build !windows

package main

import "syscall"

// restartSelf execs exe in place of this process: same PID, same terminal,
// nothing left waiting behind it. It returns only when the exec failed.
func restartSelf(exe string, args []string) (int, error) {
	return 0, syscall.Exec(exe, append([]string{exe}, args...), syscall.Environ())
}
