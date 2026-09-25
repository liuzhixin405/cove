package command

import (
	"fmt"
	"strconv"
)

// DreamUsage is the token usage and cost of the last memory consolidation.
// CostUSD 0 means the price is unknown (the model is not in the price table).
type DreamUsage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// FormatDreamCost is the /dream line for the last consolidation's usage, with
// its trailing newline, or "" when nothing is recorded. FormatDreamStatus
// (commands_dream.go) appends it once dream.Status carries the usage.
func FormatDreamCost(u DreamUsage) string {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return ""
	}
	line := fmt.Sprintf("  上次整理用量: 输入 %s / 输出 %s token", groupDigits(u.InputTokens), groupDigits(u.OutputTokens))
	if u.CostUSD > 0 {
		line += fmt.Sprintf("，约 $%.4f", u.CostUSD)
	}
	return line + "\n"
}

// groupDigits formats n with thousands separators (12345 -> "12,345").
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	neg := n < 0
	if neg {
		s = s[1:]
	}
	var out []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
