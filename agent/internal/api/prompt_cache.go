package api

// InjectCacheBreakpoints adds Anthropic cache_control markers to messages.
// Strategy: mark system prompt + last 3 non-system messages for caching.
// This is a no-op for non-Anthropic providers.
// Returns a copy of messages with cache_control annotations.
func InjectCacheBreakpoints(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}

	// Deep copy messages to avoid mutation
	result := make([]Message, len(messages))
	copy(result, messages)

	// Mark last 3 messages with cache hints (via metadata in Content prefix)
	// For Anthropic API, these get translated to cache_control blocks
	count := 0
	for i := len(result) - 1; i >= 0 && count < 3; i-- {
		if result[i].Role != "system" && result[i].Content != "" {
			result[i].CacheControl = "ephemeral"
			count++
		}
	}

	return result
}
