package main

import "strings"

// isContinueCommand reports whether the input is a "继续"/"continue" request,
// used by the TUI to trigger interrupted-draft / most-relevant-session recovery.
// (Relocated here from the now-removed classic REPL task runner.)
//
// Only the bare word counts, optionally followed by punctuation ("继续。",
// "continue!"). Any line starting with "继续" or "continue " used to match,
// and the resume path then dropped the rest of it: "继续把 README 翻译成英文"
// resumed some other session and sent it a bare "继续", and "continue with the
// tests" typed during a task was refused instead of steered into it. A longer
// line is a normal message.
func isContinueCommand(input string) bool {
	v := strings.TrimSpace(strings.ToLower(input))
	v = strings.TrimRight(v, " \t。.!！~～…,，")
	return v == "继续" || v == "continue"
}
