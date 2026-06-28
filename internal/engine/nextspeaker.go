package engine

import (
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
)

// NextSpeaker predicts whether the assistant should continue generating
// or whether the turn should end and yield to the user. It looks for
// termination signals in the latest assistant message and tool results.
type NextSpeaker struct {
	maxIterations      int
	terminationPhrases []string
}

// NewNextSpeaker creates a next-speaker predictor with defaults.
func NewNextSpeaker() *NextSpeaker {
	return &NextSpeaker{
		maxIterations: 50,
		terminationPhrases: []string{
			"task complete", "任务完成", "done", "finished",
			"no further", "nothing else", "all done",
		},
	}
}

// ShouldContinue examines recent messages and returns true if the model
// should keep generating (tool calls present, task not done).
func (ns *NextSpeaker) ShouldContinue(messages []api.Message) bool {
	if len(messages) == 0 {
		return false
	}

	last := messages[len(messages)-1]

	// If last message has tool calls, definitely continue
	if len(last.ToolCalls) > 0 {
		return true
	}

	// If last message is assistant and contains termination signal, stop
	if last.Role == "assistant" {
		if ns.CheckForTermination(messages) {
			return false
		}
	}

	// If we have a user message at the end, it's time to yield
	if last.Role == "user" {
		return false
	}

	return true
}

// CheckForTermination scans recent messages for termination signals.
func (ns *NextSpeaker) CheckForTermination(messages []api.Message) bool {
	// Check last 3 messages for termination phrases
	start := len(messages) - 3
	if start < 0 {
		start = 0
	}
	for _, msg := range messages[start:] {
		lower := strings.ToLower(msg.Content)
		for _, phrase := range ns.terminationPhrases {
			if searchInString(lower, phrase) {
				return true
			}
		}
	}
	return false
}

func containsSubstring(s, substr string) bool {
	return strings.Contains(s, substr)
}

func searchInString(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
