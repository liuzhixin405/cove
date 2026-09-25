//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stdinIsMSYSPty reports whether stdin is Git Bash's (mintty) or Cygwin's
// terminal pipe. Without this check "cove -p" run from mintty would take the
// terminal for piped input and wait for data the user never types.
func stdinIsMSYSPty() bool {
	h := windows.Handle(os.Stdin.Fd())
	if t, err := windows.GetFileType(h); err != nil || t != windows.FILE_TYPE_PIPE {
		return false
	}
	// FILE_NAME_INFO: a uint32 byte length followed by the UTF-16 name.
	buf := make([]byte, 4+windows.MAX_PATH*2)
	//nolint:gosec // len(buf) is the fixed size above (a few hundred bytes), never near uint32's range
	if err := windows.GetFileInformationByHandleEx(h, windows.FileNameInfo, &buf[0], uint32(len(buf))); err != nil {
		return false
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0])) / 2 //nolint:gosec // FILE_NAME_INFO layout requires reinterpreting the byte buffer
	if n == 0 || int(n) > windows.MAX_PATH {
		return false
	}
	name := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[4])), n) //nolint:gosec // FILE_NAME_INFO layout requires reinterpreting the byte buffer
	return isMSYSPtyPipeName(windows.UTF16ToString(name))
}
