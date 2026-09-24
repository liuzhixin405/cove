package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// RepairToolArguments parses a (possibly malformed) JSON "arguments" string
// coming back from a model's tool call. Mid-tier / domestic models routed
// through the OpenAI-compatible protocol are more likely than Claude's
// native tool_use to truncate or mis-escape long arguments (e.g. a large
// code block or multi-line diff passed to edit/write), especially when a
// response is cut short by a token limit.
//
// The previous behavior at every call site was to silently drop the entire
// tool call on any json.Unmarshal error. That throws away a real intent the
// model already spent tokens producing, and gives the model no signal that
// anything went wrong (from its point of view, it just never sees a result
// for that call). RepairToolArguments strips stray text around an otherwise
// complete object before giving up.
//
// It deliberately does not "close off" truncated arguments any more. It used
// to append the missing quotes and braces, which turned a write or edit cut
// off by the output limit into a valid call carrying half the content, and
// the tool then wrote that half to disk. Truncated arguments now fail, and
// the model is asked to resend the call.
//
// It returns the parsed arguments and true on success (either a clean parse
// or a successful repair). On failure it returns nil and false; callers
// should surface a diagnostic tool-result error to the model instead of
// silently discarding the call (see Engine.executeTool's ParseError handling).
func RepairToolArguments(raw string) (map[string]any, bool) {
	if args, ok := tryUnmarshalObject(raw); ok {
		return args, true
	}

	if extracted := extractBalancedObject(strings.TrimSpace(raw)); extracted != "" {
		if args, ok := tryUnmarshalObject(extracted); ok {
			return args, true
		}
	}

	return nil, false
}

// toolArgsParseError is the Input of a call whose arguments could not be
// parsed; Engine.executeTool reports the message to the model. truncated
// says the response hit the output token limit, which is almost always why
// the arguments are incomplete, so the model is told to send less at once.
func toolArgsParseError(raw string, truncated bool) map[string]any {
	msg := fmt.Sprintf("tool call arguments were not valid JSON and could not be auto-repaired (%d bytes, starts with: %s)",
		len(raw), truncate(raw, 120))
	if truncated {
		msg += "; the response hit the output token limit and cut the arguments off, so split the work into smaller calls (for example write a large file in several parts)"
	}
	return map[string]any{"_cove_parse_error": msg}
}

// newToolCallID makes an id for a tool call the provider sent without one.
// The tool result must name the call it answers, and an empty tool_call_id
// gets the next request rejected.
func newToolCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// tryUnmarshalObject parses s as a JSON object. An empty/whitespace-only
// string is treated as "no arguments" (valid for zero-arg tools) rather than
// an error.
func tryUnmarshalObject(s string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil, false
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true
}

// extractBalancedObject returns the longest string-aware, brace-balanced
// substring starting at the first "{" in s. Returns "" if s has no "{".
// Some providers wrap the real object in stray leading/trailing tokens (a
// leftover newline, or the start of a second call that never completed).
func extractBalancedObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}

	depth := 0
	inStr := false
	escaped := false
	lastBalancedEnd := -1

	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				lastBalancedEnd = i
			}
		}
	}

	if lastBalancedEnd < 0 {
		return "" // no complete object: the arguments were cut off
	}
	return s[start : lastBalancedEnd+1]
}
