package adapter

// ToolCall is the normalized tool-call model used by provider adapters.
type ToolCall struct {
	ID         string
	Name       string
	Input      map[string]any
	ParseError bool
	// Extra is provider data returned with the call as is (api.ToolCall.Extra).
	Extra []byte
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
