package repl

// Terminal cell widths and the part of the input line that fits.

import (
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// runeCellWidth is how many terminal columns r takes on the input line: the
// measure of textutil.Width, so the editor and the rest of the interface
// agree (East Asian Ambiguous counts 2, the direction that cannot wrap). It
// was a hand-kept table that counted Ambiguous as 1: in a CJK terminal a
// line with "·" or "…" measured short, soft-wrapped and left a ghost row.
// Newlines and tabs are the editor's own: a pasted block shows them as one
// cell (displayRunes), a bare one takes none.
func runeCellWidth(r rune) int {
	if r == 0 || r == '\n' || r == '\r' || r == '\t' {
		return 0
	}
	return textutil.RuneWidth(r)
}

func inputDisplayWindow(buf []rune, cursor, maxCols int) (disp []rune, cursorCells, used, start int) {
	if maxCols < 1 {
		maxCols = 1
	}
	// Walk back from the cursor to the widest prefix that fits. It used to
	// advance start one rune at a time and re-measure buf[start:cursor] at
	// each step, which is quadratic in the input length on every keystroke:
	// a pasted log froze the editor.
	start = cursor
	cursorCells = 0
	for start > 0 {
		cw := runeCellWidth(buf[start-1])
		if cursorCells+cw > maxCols {
			break
		}
		cursorCells += cw
		start--
	}
	end := start
	used = 0
	for end < len(buf) {
		cw := runeCellWidth(buf[end])
		if used+cw > maxCols {
			break
		}
		used += cw
		end++
	}
	disp = buf[start:end]
	return disp, cursorCells, used, start
}

func truncateAnsi(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	var sb strings.Builder
	inAnsi := false
	vis := 0
	for _, r := range s {
		if r == '\x1b' {
			inAnsi = true
			sb.WriteRune(r)
			continue
		}
		if inAnsi {
			sb.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
			continue
		}
		cw := runeCellWidth(r)
		if vis+cw > maxCols {
			break
		}
		vis += cw
		sb.WriteRune(r)
	}
	// ensure ansi resets if truncated
	sb.WriteString("\x1b[0m")
	return sb.String()
}

func truncateRunesByCells(rs []rune, maxCols int) ([]rune, int) {
	used := 0
	end := 0
	for end < len(rs) {
		cw := runeCellWidth(rs[end])
		if used+cw > maxCols {
			break
		}
		used += cw
		end++
	}
	return rs[:end], used
}

// displayRunes maps the runes of a pasted block that cannot be drawn inside a
// one-row editor: a newline would move the cursor off the row (and, being a
// bare \n in raw mode, staircase the screen), and a tab jumps to a tab stop
// the width arithmetic knows nothing about. The mapping is one rune for one,
// so cursor indexes stay valid. The buffer itself is untouched.
func displayRunes(buf []rune) []rune {
	var out []rune
	for i, r := range buf {
		if r != '\n' && r != '\t' {
			continue
		}
		if out == nil {
			out = append([]rune(nil), buf...)
		}
		if r == '\n' {
			out[i] = '↵'
		} else {
			out[i] = ' '
		}
	}
	if out == nil {
		return buf
	}
	return out
}
