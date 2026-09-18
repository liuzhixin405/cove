//go:build windows

package textmode

import "golang.org/x/sys/windows"

// consoleNeedsASCIIFallback reports whether the console's output code page is
// anything other than UTF-8.
func consoleNeedsASCIIFallback() bool {
	cp, err := windows.GetConsoleOutputCP()
	if err != nil {
		// Not a console (piped, redirected, or a terminal emulator that does
		// not implement the API). Assume UTF-8: the alternative is degrading
		// every non-Windows-console case on a failed probe.
		return false
	}
	return cp != 65001
}
