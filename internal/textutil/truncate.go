// Package textutil holds UTF-8-safe string clipping helpers.
//
// Slicing a Go string with s[:n] cuts on a byte boundary. Since much of the
// content flowing through cove (chat text, file contents, memory entries, tool
// output) contains multi-byte runes, a raw byte slice regularly lands in the
// middle of a rune and produces invalid UTF-8 — which then shows up as U+FFFD
// in the TUI or gets rejected by a provider's JSON encoder. Every clip in the
// codebase should go through one of the functions here instead.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Ellipsis is the marker appended by ClipRunes and ClipBytes.
const Ellipsis = "..."

// ClipRunes limits s to at most n runes, appending "..." when it had to clip.
// The result never exceeds n runes: the ellipsis replaces the last three of
// them, matching the historical s[:n-3]+"..." behavior for ASCII input.
//
// n < 0 returns "". n <= len(Ellipsis) clips without an ellipsis, because
// there is no room for one.
func ClipRunes(s string, n int) string {
	if n < 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= len(Ellipsis) {
		return HeadRunes(s, n)
	}
	return HeadRunes(s, n-len(Ellipsis)) + Ellipsis
}

// ClipBytes limits s to at most maxBytes bytes without splitting a rune,
// appending suffix when it had to clip. The suffix is not counted against
// maxBytes — callers use this for "content plus a [truncated] note" limits
// where the note is expected to survive.
func ClipBytes(s string, maxBytes int, suffix string) string {
	if maxBytes < 0 {
		return suffix
	}
	if len(s) <= maxBytes {
		return s
	}
	return truncAtRuneBoundary(s, maxBytes) + suffix
}

// HeadRunes returns the first n runes of s, with no ellipsis. Used where the
// clipped text feeds a comparison rather than being shown to a human.
func HeadRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// truncAtRuneBoundary returns the longest prefix of s that is at most maxBytes
// long and ends on a rune boundary.
func truncAtRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Walk back from maxBytes until the byte there starts a new rune, so the
	// prefix ends cleanly. At most 3 steps for valid UTF-8.
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// Indent prefixes every line of s with prefix. Kept here because callers that
// clip text usually format it right afterwards.
func Indent(s, prefix string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
