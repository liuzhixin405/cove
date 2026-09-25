package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove/internal/token"
)

// readNextMarkerRe matches the read tool's last line on a partial read.
var readNextMarkerRe = regexp.MustCompile(`^\[next: offset=\d+\]$`)

// readShowingRe matches the read tool's "... [showing lines a-b of n]" line.
var readShowingRe = regexp.MustCompile(`^\.\.\. \[showing lines \d+-\d+ of \d+\]$`)

// readNumberedLineRe matches a numbered content line of read output
// ("12: text"; a tab separator is accepted too).
var readNumberedLineRe = regexp.MustCompile(`^(\d+)[:\t]`)

// readMarkerReserve is the share of the limit kept for the rebuilt marker
// line and TruncateToTokens' own "[truncated]" note.
const readMarkerReserve = 32

// truncatedNote is what token.TruncateToTokens appends to a cut text.
const truncatedNote = "\n... [truncated]"

// truncateReadResult fits a read result into limit tokens and returns it with
// its continuation marker split off, for the caller to append last.
//
// The read tool ends a partial read with "[next: offset=N]". Cutting the
// result cut that line too, so the model no longer learned where to continue.
// When the engine has to cut, the marker is rebuilt from the last numbered
// line actually kept: N is that line's number + 1.
func truncateReadResult(output string, limit int) (body, marker string) {
	body, marker = splitReadMarker(output)
	if token.Estimate(output) <= limit {
		return body, marker
	}
	// The tool's own "showing lines" note describes the untruncated result.
	body = strings.TrimRight(body, "\n")
	if i := strings.LastIndexByte(body, '\n'); i >= 0 && readShowingRe.MatchString(body[i+1:]) {
		body = body[:i]
	}
	budget := limit - readMarkerReserve
	if budget < limit/2 {
		budget = limit / 2
	}
	full := body
	kept := strings.TrimSuffix(token.TruncateToTokens(full, budget), truncatedNote)
	// TruncateToTokens may stop inside a line; a partial last line is
	// dropped so the marker points at it and it is read whole next time.
	if len(kept) < len(full) && full[len(kept)] != '\n' {
		if j := strings.LastIndexByte(kept, '\n'); j >= 0 {
			kept = kept[:j]
		}
	}
	body = kept + truncatedNote
	last := lastReadLineNumber(kept)
	if last == 0 {
		return body, ""
	}
	return body, fmt.Sprintf("[next: offset=%d]", last+1)
}

// splitReadMarker removes a trailing "[next: offset=N]" line.
func splitReadMarker(output string) (body, marker string) {
	trimmed := strings.TrimRight(output, "\n")
	i := strings.LastIndexByte(trimmed, '\n')
	if i < 0 || !readNextMarkerRe.MatchString(trimmed[i+1:]) {
		return output, ""
	}
	return trimmed[:i], trimmed[i+1:]
}

// lastReadLineNumber returns the number of the last numbered line in read
// output, or 0 when there is none.
func lastReadLineNumber(body string) int {
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if m := readNumberedLineRe.FindStringSubmatch(lines[i]); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}
