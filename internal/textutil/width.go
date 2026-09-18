package textutil

import "github.com/clipperhouse/displaywidth"

// Terminal display width.
//
// # Why this is not ansi.StringWidth
//
// A character's width in columns is not a property of the character. For the
// East Asian Ambiguous class it is a property of the terminal: the same rune is
// one column in a Western locale and two in a CJK one. That class contains
// almost every symbol a terminal UI reaches for — ▸ ▾ ✓ ✗ ⎿ › · … ↑ ⚙ are all
// Ambiguous.
//
// ansi.StringWidth hardcodes the narrow interpretation. In a CJK terminal that
// under-measures every row carrying one of those glyphs, and under-measuring is
// the dangerous direction:
//
//   - A right-aligned element falls off the end. This is how "/x r1" reached
//     the screen as "/x r", making the expand command unusable — one column of
//     error, one missing character, one dead affordance.
//   - Worse, a row computed as exactly the terminal width is really one column
//     over, so the terminal soft-wraps it and the frame silently gains a row.
//     The frame's height is an exact budget, so that error does not correct
//     itself; it accumulates. That is the original "布局会随着任务进行会乱掉".
//
// So width is measured with Ambiguous = 2, which is the conservative
// direction. On a terminal that renders those glyphs narrow the result is a
// harmless one-column gap on the right of a line. On one that renders them
// wide it is exact. Neither can wrap.
//
// cove's interface is Chinese throughout, so the CJK case is the normal case
// rather than the exception.
var termWidth = displaywidth.Options{
	EastAsianWidth: true,
	// Escape sequences carry no width. Without this, a colour code would be
	// counted as the handful of printable characters it is made of.
	ControlSequences: true,
}

// Width returns the display width of s in terminal columns. ANSI escape
// sequences count as zero.
func Width(s string) int { return termWidth.String(s) }

// TruncateWidth clips s to at most width columns, appending tail when it had
// to cut. The result including tail never exceeds width.
//
// It measures the same way Width does, which is the point: a truncation that
// disagrees with the width check it was meant to satisfy just moves the
// off-by-one somewhere else.
func TruncateWidth(s string, width int, tail string) string {
	if width <= 0 {
		return ""
	}
	// A tail that does not itself fit is dropped. Without this the result
	// comes back wider than the budget — "…" is two columns under the
	// conservative reading, so asking for one column returned two, and the
	// caller's own width check would then have to re-truncate what the
	// truncator produced.
	if Width(tail) >= width {
		tail = ""
	}
	return termWidth.TruncateString(s, width, tail)
}
