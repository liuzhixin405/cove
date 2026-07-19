package engine

import "github.com/liuzhixin405/cove/internal/api"

// hasToolCalls reports whether the model requested one or more tool calls.
func hasToolCalls(resp *api.ChatResponse) bool {
	return resp != nil && len(resp.ToolCalls) > 0
}

// assistantMessageFromResponse converts a model response to an assistant message.
func assistantMessageFromResponse(resp *api.ChatResponse) api.Message {
	if resp == nil {
		return api.Message{Role: "assistant"}
	}
	return api.Message{
		Role:             "assistant",
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        resp.ToolCalls,
	}
}
