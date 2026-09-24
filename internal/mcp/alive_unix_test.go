//go:build !windows

package mcp

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether pid names a running (non-zombie) process.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	// An orphan that was killed stays a zombie until init reaps it.
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}
