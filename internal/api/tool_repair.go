package api

import (
	"encoding/json"
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
// for that call). RepairToolArguments tries a couple of cheap, deterministic
// repairs before giving up, so a one-character formatting slip doesn't cost
// an entire turn.
//
// It returns the parsed arguments and true on success (either a clean parse
// or a successful repair). On failure it returns nil and false; callers
// should surface a diagnostic tool-result error to the model instead of
// silently discarding the call (see Engine.executeTool's ParseError handling).
func RepairToolArguments(raw string) (map[string]any, bool) {
	if args, ok := tryUnmarshalObject(raw); ok {
		return args, true
	}

	if repaired := repairJSONObject(raw); repaired != "" {
		if args, ok := tryUnmarshalObject(repaired); ok {
			return args, true
		}
	}

	return nil, false
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

// repairJSONObject attempts a small number of deterministic fixes for the
// most common truncation/formatting problems seen in tool-call arguments,
// returning a best-effort candidate string. The caller re-validates the
// result by attempting to unmarshal it again; repairJSONObject itself never
// panics and returns "" if it cannot produce any candidate worth trying.
func repairJSONObject(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// Some providers wrap the real object in stray leading/trailing tokens
	// (e.g. a leftover newline, or the start of a second call that never
	// completed). Extract the first balanced {...} block if present.
	if extracted := extractBalancedObject(s); extracted != "" {
		s = extracted
	}

	// The most common real-world failure is a response cut off mid-value
	// (token limit hit while streaming a long string field). Try to close
	// off any unterminated string/array/object so the fields that did arrive
	// intact are not lost.
	return closeUnterminated(s)
}

// extractBalancedObject returns the longest string-aware, brace-balanced
// substring starting at the first "{" in s. Returns "" if s has no "{".
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

	if lastBalancedEnd >= 0 {
		return s[start : lastBalancedEnd+1]
	}
	// No fully-balanced object found; return from the opening brace onward
	// so closeUnterminated can still try to close it off.
	return s[start:]
}

// closeUnterminated walks s tracking open string/brace/bracket state and
// appends the minimum set of closing tokens needed to make it syntactically
// well-formed. It does not attempt to recover the *content* of a value that
// was cut off mid-stream — only to stop that truncation from invalidating
// every field that arrived before it. A trailing dangling comma/colon (a
// common truncation artifact right before a cut-off key or value) is
// trimmed before closing.
func closeUnterminated(s string) string {
	if s == "" {
		return ""
	}

	inStr := false
	escaped := false
	var stack []byte // '{' or '['

	for i := 0; i < len(s); i++ {
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
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	result := s
	if inStr {
		result += `"`
	}
	result = strings.TrimRight(result, " \t\r\n")
	result = strings.TrimRight(result, ",")
	result = strings.TrimRight(result, ":")

	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i] {
		case '{':
			result += "}"
		case '[':
			result += "]"
		}
	}
	return result
}
