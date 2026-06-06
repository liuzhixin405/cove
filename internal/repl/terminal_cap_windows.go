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

	// Only require VT processing to be enabled — any console host that supports
	// VT sequences (Windows Terminal, ConEmu, VSCode, IntelliJ, etc.) qualifies.
	// We no longer restrict to WT_SESSION because many capable terminals don't
	// set that variable.
	stdout := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(stdout, &mode); err != nil {
		return true
	}
	return mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0
}

func forcePlainReadline() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("COVE_PLAIN_REPL")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
