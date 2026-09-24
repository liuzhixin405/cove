package tool

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// replaceBlocked reports whether a rename failed because another process has
// the destination open. Windows reports that as a sharing or lock violation,
// or as access denied when the holder did not grant FILE_SHARE_DELETE (Go's
// own os.Open does not).
func replaceBlocked(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// hardLinkCount returns the number of names the file has. os.Stat does not
// expose it on Windows, so it takes an open handle; when that fails it assumes
// one name, the common case.
func hardLinkCount(path string, _ os.FileInfo) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer func() { _ = f.Close() }()
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &fi); err != nil {
		return 1
	}
	return uint64(fi.NumberOfLinks)
}
