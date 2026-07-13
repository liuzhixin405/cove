//go:build windows

package tui

import "golang.org/x/sys/windows"

func windowsConsoleNeedsASCIIFallback() bool {
	cp, err := windows.GetConsoleOutputCP()
	if err != nil {
		return false
	}
	return cp != 65001
}
