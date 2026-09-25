//go:build windows

package dream

import "syscall"

// detachedProcess is DETACHED_PROCESS: the worker gets no console, so closing
// the terminal cove ran in does not end it.
const detachedProcess = 0x00000008

// detachedAttr puts the worker in its own process group (Ctrl+C in the
// parent's console does not reach it) with no console.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
}
