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

// TruncateKeepTail keeps text within about maxTokens by dropping lines from
// the middle while keeping the last tailLines lines verbatim and as much of
// the start as fits. It suits results whose first line names the outcome
// ("Error: ...") and whose last lines carry the conclusion (exit status, the
// final failure), where cutting either end loses the point.
func TruncateKeepTail(text string, maxTokens int, tailLines int) string {
	if maxTokens <= 0 {
		return "... [truncated]"
	}
	if Estimate(text) <= maxTokens {
		return text
	}
	if tailLines < 0 {
		tailLines = 0
	}
	tailStart := len(text)
	for n := 0; n < tailLines && tailStart > 0; n++ {
		i := strings.LastIndexByte(text[:tailStart-1], '\n')
		if i < 0 {
			tailStart = 0
			break
		}
		tailStart = i + 1
	}
	// Room for the omission marker, which Estimate counts too.
	const markerReserve = 16
	tail := text[tailStart:]
	headBudget := maxTokens - Estimate(tail) - markerReserve
	if tailStart == 0 || headBudget < maxTokens/4 {
		// The tail alone would crowd out the head; keep both ends evenly.
		return TruncateMiddle(text, maxTokens-markerReserve)
	}
	headEnd := prefixEnd(text, headBudget)
	if headEnd >= tailStart {
		return text
	}
	omitted := strings.Count(text[headEnd:tailStart], "\n")
	return text[:headEnd] + fmt.Sprintf("\n... [%d lines omitted] ...\n", omitted) + tail
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
