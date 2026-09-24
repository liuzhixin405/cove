package token

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Estimate returns an approximate token count without allocating.
// Uses ~3 ASCII bytes per token (code/symbols density) and one token per non-ASCII rune.
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
	return (asciiBytes+2)/3 + nonASCII
}

// TruncateToTokens truncates text to approximately maxTokens.
func TruncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return "... [truncated]"
	}
	if Estimate(text) <= maxTokens {
		return text
	}
	return text[:prefixEnd(text, maxTokens)] + "\n... [truncated]"
}

// TruncateMiddle keeps the start and the end of text within about maxTokens
// and drops the middle. Command output needs both ends: the first lines show
// what ran, the last ones hold the failure summary, stderr and exit status.
func TruncateMiddle(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return "... [truncated]"
	}
	if Estimate(text) <= maxTokens {
		return text
	}
	headTokens := maxTokens * 2 / 5
	headEnd := prefixEnd(text, headTokens)
	tailStart := suffixStart(text, maxTokens-headTokens)
	if tailStart <= headEnd {
		return text
	}
	omitted := strings.Count(text[headEnd:tailStart], "\n")
	return text[:headEnd] + fmt.Sprintf("\n... [%d lines omitted] ...\n", omitted) + text[tailStart:]
}

// prefixEnd returns the byte offset where the first maxTokens tokens end,
// moved back to a line or word boundary when one is near.
func prefixEnd(text string, maxTokens int) int {
	tokens := 0
	end := 0
	foundEnd := false
	asciiRun := 0
	for i, r := range text {
		if r < utf8.RuneSelf {
			asciiRun++
			if asciiRun == 3 {
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
	return end
}

// suffixStart returns the byte offset where the last maxTokens tokens begin,
// moved forward to the next line start when one is near.
func suffixStart(text string, maxTokens int) int {
	tokens := 0
	asciiRun := 0
	i := len(text)
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:i])
		if r < utf8.RuneSelf {
			asciiRun++
			if asciiRun == 3 {
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
			break
		}
		i -= size
	}
	if j := strings.IndexByte(text[i:], '\n'); j >= 0 && j < 80 {
		i += j + 1
	}
	return i
}
