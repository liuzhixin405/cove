//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// procTree runs a stdio server in its own process group, so killing it takes
// down whatever it spawned too. Servers are often launched through a wrapper
// (uvx, npx shims, docker run) and Process.Kill only reached the wrapper,
// orphaning the real server.
type procTree struct {
	pgid int
}

func setupProcTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func (p *procTree) attach(cmd *exec.Cmd) {
	p.pgid = cmd.Process.Pid
}

// kill terminates the whole group.
func (p *procTree) kill(cmd *exec.Cmd) {
	if p.pgid > 0 {
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
}

// release kills anything the server left behind in its group after it exited.
func (p *procTree) release() {
	if p.pgid > 0 {
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
		p.pgid = 0
	}
}
