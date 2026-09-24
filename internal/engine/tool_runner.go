package engine

import "github.com/liuzhixin405/cove/internal/api"

// hasToolCalls reports whether the model requested one or more tool calls.
func hasToolCalls(resp *api.ChatResponse) bool {
	return resp != nil && len(resp.ToolCalls) > 0
}

// assistantMessageFromResponse converts a model response to an assistant message.
// syntheticToolResults builds one tool-result message per tool call in the
// assistant turn, all carrying the same note.
//
// Both Anthropic and OpenAI require every tool_use block in an assistant
// message to be answered by a matching tool_result before any other message
// can follow. Whenever the engine decides NOT to execute a batch of tool calls
// (loop detection aborting the batch, for instance), it still has to close them
// out — otherwise the next request carries assistant(tool_use) → user, which
// the provider rejects with a 400 and the whole turn fails hard. That turned the
// loop-recovery path into a guaranteed failure exactly when it was needed.
func syntheticToolResults(toolCalls []api.ToolCall, note string) []api.Message {
	if len(toolCalls) == 0 {
		return nil
	}
	msgs := make([]api.Message, 0, len(toolCalls))
	for _, tc := range toolCalls {
		msgs = append(msgs, api.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    note,
		})
	}
	return msgs
}

func assistantMessageFromResponse(resp *api.ChatResponse) api.Message {
	if resp == nil {
		return api.Message{Role: "assistant"}
	}
	return api.Message{
		Role:             "assistant",
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        resp.ToolCalls,
		// Must go back verbatim with the tool results, or the API rejects
		// the continuation.
		ThinkingBlocks: resp.ThinkingBlocks,
	}
}
