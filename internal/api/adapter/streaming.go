package adapter

import "strings"

// StreamAccumulator normalizes provider streaming chunks into one aggregated result.
//
// Text is kept in strings.Builders: appending each delta to a string used to
// copy the whole text so far on every chunk, which is quadratic for the tens
// of thousands of tiny deltas in a long answer or reasoning trace.
type StreamAccumulator struct {
	content   strings.Builder
	reasoning strings.Builder
	toolCalls []ToolCall
}

func (a *StreamAccumulator) AddDelta(delta string) {
	a.content.WriteString(delta)
}

func (a *StreamAccumulator) AddReasoning(reasoning string) {
	a.reasoning.WriteString(reasoning)
}

func (a *StreamAccumulator) AddToolCall(tc ToolCall) {
	a.toolCalls = append(a.toolCalls, tc)
}

func (a *StreamAccumulator) Content() string {
	return a.content.String()
}

func (a *StreamAccumulator) Reasoning() string {
	return a.reasoning.String()
}

func (a *StreamAccumulator) ToolCalls() []ToolCall {
	return CloneToolCalls(a.toolCalls)
}
