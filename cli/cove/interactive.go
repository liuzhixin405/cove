package main

import (
	"os"

	"github.com/charmbracelet/x/term"
)

// useInteractiveShell reports whether to run the interactive shell rather than
// the headless front end.
//
// It is the default; it is skipped when explicitly disabled (--no-tui or
// COVE_TUI=0) or when stdin/stdout is not a terminal, where the headless front
// end is correct because there is no input line to edit and no terminal to
// draw on. --tui or COVE_TUI=1 force it on even in those cases.
//
// The flag names predate the shell rewrite and are kept for compatibility.
func useInteractiveShell() bool {
	if noTUI || os.Getenv("COVE_TUI") == "0" {
		return false
	}
	if tuiMode || os.Getenv("COVE_TUI") == "1" {
		return true
	}
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// toolsInteractiveFor reports whether a run in this mode has someone to
// answer the question tool: not -p, and the interactive shell rather than
// the piped/headless frontend (the branch main takes after bootstrap).
func toolsInteractiveFor(printMode bool) bool {
	return !printMode && useInteractiveShell()
}
