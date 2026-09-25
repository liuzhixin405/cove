//go:build windows

package mcp

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/liuzhixin405/cove/internal/log"
)

// procTree ties a stdio server and everything it spawns to a job object.
//
// On Windows most servers are started through a wrapper - npx is npx.cmd, so
// the chain is cmd.exe -> node -> the server - and Process.Kill only reaches
// the wrapper; the real server was orphaned and kept running. A job object
// with KILL_ON_JOB_CLOSE takes the whole tree down, both when Close kills it
// and when cove itself exits or crashes (the OS closes the job handle).
type procTree struct {
	job windows.Handle
}

func setupProcTree(cmd *exec.Cmd) {}

// attach puts the started process into a fresh job. Failure is not fatal: the
// server still runs, only tree-kill falls back to killing the direct child.
func (p *procTree) attach(cmd *exec.Cmd) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Debugf("mcp: CreateJobObject: %v", err)
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	//nolint:gosec // Windows job-object API requires passing the struct by unsafe pointer
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		log.Debugf("mcp: SetInformationJobObject: %v", err)
		_ = windows.CloseHandle(job)
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)) //nolint:gosec // a live process's PID is a small positive OS handle value, never near uint32's range
	if err != nil {
		log.Debugf("mcp: OpenProcess: %v", err)
		_ = windows.CloseHandle(job)
		return
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		log.Debugf("mcp: AssignProcessToJobObject: %v", err)
		_ = windows.CloseHandle(job)
		return
	}
	p.job = job
}

// kill terminates the whole tree.
func (p *procTree) kill(cmd *exec.Cmd) {
	if p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
	}
	_ = cmd.Process.Kill()
}

// release closes the job, which also kills anything the server left behind.
func (p *procTree) release() {
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}
