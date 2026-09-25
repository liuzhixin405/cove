//go:build windows

package fsatomic

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isRetryableRename reports whether a rename failed only because another
// process has the destination open.
func isRetryableRename(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
