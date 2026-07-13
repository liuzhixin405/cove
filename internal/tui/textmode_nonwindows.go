//go:build !windows

package tui

func windowsConsoleNeedsASCIIFallback() bool {
	return false
}
