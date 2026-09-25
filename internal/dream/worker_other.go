//go:build !windows

package dream

import "syscall"

// detachedAttr starts the worker in a new session, so the terminal's hangup
// and job-control signals do not reach it when cove exits.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
