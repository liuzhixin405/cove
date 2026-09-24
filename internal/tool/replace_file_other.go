//go:build !windows

package tool

import (
	"os"
	"syscall"
)

// replaceBlocked is Windows-only: a POSIX rename replaces a file whoever has
// it open.
func replaceBlocked(error) bool { return false }

// hardLinkCount returns the number of names the file has, from the stat
// replaceFile already made.
func hardLinkCount(_ string, info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
