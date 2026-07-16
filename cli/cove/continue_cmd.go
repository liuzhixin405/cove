package main

import "strings"

// isContinueCommand reports whether the input is a "继续"/"continue" request,
// used by the TUI to trigger interrupted-draft / most-relevant-session recovery.
// (Relocated here from the now-removed classic REPL task runner.)
func isContinueCommand(input string) bool {
	v := strings.TrimSpace(strings.ToLower(input))
	if v == "继续" || v == "continue" {
		return true
	}
	return strings.HasPrefix(v, "继续") || strings.HasPrefix(v, "continue ")
}
