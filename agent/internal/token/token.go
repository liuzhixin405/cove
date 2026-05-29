package token

import "strconv"

// Estimate returns an approximate token count without allocating.
// Uses a character-based heuristic: ~4 chars per token for English/code.
func Estimate(text string) int {
	if len(text) == 0 {
		return 0
	}
	// Fast path: pure character-based estimation (no allocation)
	return len(text)/4 + 1
}

func EstimateMessages(messages []string) int {
	total := 0
	for _, m := range messages {
		total += Estimate(m)
	}
	return total
}

// TruncateToTokens truncates text to approximately maxTokens.
// Uses byte-offset estimation to avoid allocating a word slice.
func TruncateToTokens(text string, maxTokens int) string {
	if Estimate(text) <= maxTokens {
		return text
	}
	// Approximate cutoff: maxTokens * 4 characters
	cutoff := maxTokens * 4
	if cutoff >= len(text) {
		return text
	}
	// Find last newline or space near cutoff to avoid splitting mid-word
	end := cutoff
	for end > cutoff-80 && end > 0 {
		if text[end] == '\n' || text[end] == ' ' {
			break
		}
		end--
	}
	if end <= 0 {
		end = cutoff
	}
	return text[:end] + "\n... [truncated]"
}

func TruncateBytes(data string, maxBytes int) string {
	if len(data) <= maxBytes {
		return data
	}
	return data[:maxBytes] + "\n... [truncated " + strconv.Itoa(len(data)-maxBytes) + " bytes]"
}
