package adapter

// ToolCall is the normalized tool-call model used by provider adapters.
type ToolCall struct {
	ID         string
	Name       string
	Input      map[string]any
	ParseError bool
}

// CloneToolCalls returns a defensive copy of tool calls.
func CloneToolCalls(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(calls))
	copy(out, calls)
	return out
}

// HasParseError reports whether any tool-call failed argument parsing.
func HasParseError(calls []ToolCall) bool {
	for i := range calls {
		if calls[i].ParseError {
			return true
		}
	}
	return false
}
