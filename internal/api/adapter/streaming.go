package adapter

// StreamAccumulator normalizes provider streaming chunks into one aggregated result.
type StreamAccumulator struct {
	content   string
	reasoning string
	toolCalls []ToolCall
}

func (a *StreamAccumulator) AddDelta(delta string) {
	a.content += delta
}

func (a *StreamAccumulator) AddReasoning(reasoning string) {
	a.reasoning += reasoning
}

func (a *StreamAccumulator) AddToolCall(tc ToolCall) {
	a.toolCalls = append(a.toolCalls, tc)
}

func (a *StreamAccumulator) Content() string {
	return a.content
}

func (a *StreamAccumulator) Reasoning() string {
	return a.reasoning
}

func (a *StreamAccumulator) ToolCalls() []ToolCall {
	return CloneToolCalls(a.toolCalls)
}
