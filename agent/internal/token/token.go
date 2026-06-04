package token

import (
	"strconv"
	"unicode/utf8"
)

// Estimate returns an approximate token count without allocating.
// Uses ~4 ASCII bytes per token and one token per non-ASCII rune.
func Estimate(text string) int {
	if len(text) == 0 {
		return 0
	}
	asciiBytes := 0
	nonASCII := 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			asciiBytes++
		} else {
			nonASCII++
		}
	}
	return (asciiBytes+3)/4 + nonASCII
}

func EstimateMessages(messages []string) int {
	total := 0
	for _, m := range messages {
		total += Estimate(m)
	}
	return total
}

// TruncateToTokens truncates text to approximately maxTokens.
func TruncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return "... [truncated]"
	}
	if Estimate(text) <= maxTokens {
		return text
	}
	tokens := 0
	end := 0
	foundEnd := false
	asciiRun := 0
	for i, r := range text {
		if r < utf8.RuneSelf {
			asciiRun++
			if asciiRun == 4 {
				tokens++
				asciiRun = 0
			}
		} else {
			if asciiRun > 0 {
				tokens++
				asciiRun = 0
			}
			tokens++
		}
		if tokens >= maxTokens {
			end = i
			foundEnd = true
			break
		}
	}
	if !foundEnd {
		end = len(text)
	}
	if end > 80 {
		for i := end; i > end-80 && i > 0; i-- {
			if text[i] == '\n' || text[i] == ' ' {
				end = i
				break
			}
		}
	}
	return text[:end] + "\n... [truncated]"
}

func TruncateBytes(data string, maxBytes int) string {
	if len(data) <= maxBytes {
		return data
	}
	return data[:maxBytes] + "\n... [truncated " + strconv.Itoa(len(data)-maxBytes) + " bytes]"
}
