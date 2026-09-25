package engine

import (
	"github.com/liuzhixin405/cove/internal/api"
)

func chooseCompressionSplitAssistant(messages []api.Message, keepCount int) int {
	splitIdx := len(messages) - keepCount
	if splitIdx <= 0 || splitIdx >= len(messages) {
		return -1
	}
	for splitIdx > 0 && splitIdx < len(messages) && messages[splitIdx].Role != "assistant" {
		splitIdx--
	}
	if splitIdx <= 0 {
		return -1
	}
	return splitIdx
}
