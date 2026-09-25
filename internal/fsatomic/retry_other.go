//go:build !windows

package fsatomic

// isRetryableRename is false off Windows: POSIX rename replaces a file that
// another process has open, so a failure there is not transient.
func isRetryableRename(error) bool { return false }
