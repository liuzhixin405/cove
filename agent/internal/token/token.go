package token

import "strings"

func Estimate(text string) int {
	if len(text) == 0 {
		return 0
	}
	words := strings.Fields(text)
	tokens := 0
	for _, w := range words {
		tokens += 1 + len(w)/4
	}
	return max(tokens, len(text)/4)
}

func EstimateMessages(messages []string) int {
	total := 0
	for _, m := range messages {
		total += Estimate(m)
	}
	return total
}

func TruncateToTokens(text string, maxTokens int) string {
	if Estimate(text) <= maxTokens {
		return text
	}
	words := strings.Fields(text)
	tokens := 0
	for i, w := range words {
		tokens += 1 + len(w)/4
		if tokens > maxTokens {
			return strings.Join(words[:i], " ") + "\n... [truncated]"
		}
	}
	return text
}

func TruncateBytes(data string, maxBytes int) string {
	if len(data) <= maxBytes {
		return data
	}
	return data[:maxBytes] + "\n... [truncated " + itoa(len(data)-maxBytes) + " bytes]"
}

func max(a, b int) int {
	if a > b { return a }
	return b
}

func itoa(n int) string {
	if n == 0 { return "0" }
	d := []byte{}
	for n > 0 { d = append([]byte{byte('0'+n%10)}, d...); n /= 10 }
	return string(d)
}
