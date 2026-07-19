package adapter

// Message is the normalized message model used by provider adapters.
type Message struct {
	Role             string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	ToolCallID       string
	Name             string
}
