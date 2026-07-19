package engine

import "github.com/liuzhixin405/cove/internal/api"

// streamCallbacks bundles optional stream event callbacks.
type streamCallbacks struct {
	onDelta     func(string)
	onReasoning func(string)
}

func emitStreamEvent(cb streamCallbacks, ev api.StreamEvent) {
	if ev.Type == "delta" && cb.onDelta != nil {
		cb.onDelta(ev.Delta)
		return
	}
	if ev.Type == "reasoning" && cb.onReasoning != nil {
		cb.onReasoning(ev.Reasoning)
	}
}
