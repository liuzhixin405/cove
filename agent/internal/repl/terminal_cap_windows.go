//go:build windows

package repl

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func shouldUseFallbackReadline() bool {
	if forcePlainReadline() {
		return true
	}
	if !isWindowsTerminal() {
		return true
	}

	// If VT processing is not enabled, ANSI redraw can leave visual artifacts
	// in some Windows console hosts when pasting long lines.
	stdout := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(stdout, &mode); err != nil {
		return true
	}
	return mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0
}

func isWindowsTerminal() bool {
	return strings.TrimSpace(os.Getenv("WT_SESSION")) != ""
}

func forcePlainReadline() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("AGENTGO_PLAIN_REPL")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
