package render

import (
	"fmt"
	"strings"
)

// LineDiff is a unified line diff of two texts, for showing what an edit or
// a write did (the tool block's expanded form, the permission preview).
type LineDiff struct {
	// Text is the diff: "@@ -a,b +c,d @@" hunks with 3 lines of context,
	// "-" and "+" lines. Empty only when the texts are equal. A line that
	// ends its text without a newline is followed by "\ No newline at end of
	// file", as in diff -u, and a change of line endings (CRLF/LF) the lines
	// cannot show is stated in a leading "\ Line endings changed..." line.
	Text string
	// Added and Removed count the "+" and "-" lines.
	Added, Removed int
}

// Summary is the "+12 −3" line of the diff.
func (d LineDiff) Summary() string {
	return fmt.Sprintf("+%d −%d", d.Added, d.Removed)
}

const (
	diffContext = 3
	// diffMaxCells bounds the LCS table (old lines × new lines of the part
	// that differs). Past it the differing middle is shown as one removal
	// and one addition: still correct, only less minimal.
	diffMaxCells = 4_000_000
)

// Diff computes the line diff from old to newText.
func Diff(old, newText string) LineDiff {
	if old == newText {
		return LineDiff{}
	}
	a, crA := splitDiffLines(old)
	b, crB := splitDiffLines(newText)

	// Common prefix and suffix need no table.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{kind: ' ', text: a[i], ai: i, bi: i})
	}
	ops = append(ops, middleOps(a[pre:len(a)-suf], b[pre:len(b)-suf], pre, pre)...)
	for i := 0; i < suf; i++ {
		ai, bi := len(a)-suf+i, len(b)-suf+i
		ops = append(ops, diffOp{kind: ' ', text: a[ai], ai: ai, bi: bi})
	}
	d := formatHunks(ops)
	// The note goes first so a preview cut to its first lines still shows it.
	if note := lineEndingNote(ops, a, b, crA, crB); note != "" {
		if d.Text == "" {
			d.Text = note
		} else {
			d.Text = note + "\n" + d.Text
		}
	}
	return d
}

type diffOp struct {
	kind   byte // ' ', '-', '+'
	text   string
	ai, bi int // line index in old / new (for '-' only ai, '+' only bi)
}

// middleOps diffs the differing middle with an LCS table.
func middleOps(a, b []string, aOff, bOff int) []diffOp {
	n, m := len(a), len(b)
	var ops []diffOp
	if n*m > diffMaxCells {
		for i, l := range a {
			ops = append(ops, diffOp{kind: '-', text: l, ai: aOff + i})
		}
		for j, l := range b {
			ops = append(ops, diffOp{kind: '+', text: l, bi: bOff + j})
		}
		return ops
	}
	// lcs[i][j] = LCS length of a[i:], b[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, diffOp{kind: ' ', text: a[i], ai: aOff + i, bi: bOff + j})
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] > lcs[i+1][j]):
			ops = append(ops, diffOp{kind: '+', text: b[j], bi: bOff + j})
			j++
		default:
			ops = append(ops, diffOp{kind: '-', text: a[i], ai: aOff + i})
			i++
		}
	}
	return ops
}

// formatHunks renders ops as unified hunks with diffContext lines around
// each change.
func formatHunks(ops []diffOp) LineDiff {
	var d LineDiff
	var sb strings.Builder
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// A hunk: from diffContext lines before this change to diffContext
		// after the last change within 2*diffContext of the previous one.
		start := max(0, i-diffContext)
		end := i
		for k := i; k < len(ops); k++ {
			if ops[k].kind != ' ' {
				end = k
				continue
			}
			if k-end > 2*diffContext {
				break
			}
		}
		stop := min(len(ops), end+diffContext+1)
		aStart, bStart, aLen, bLen := -1, -1, 0, 0
		var body strings.Builder
		for _, op := range ops[start:stop] {
			switch op.kind {
			case ' ':
				// Each side takes its start from its own first line. Both
				// used to be set together whenever the old side had none
				// yet, so a hunk opening with an addition had the new side's
				// start overwritten by the context line after it: prepending
				// a line gave "+2,3" instead of "+1,3".
				if aStart < 0 {
					aStart = op.ai
				}
				if bStart < 0 {
					bStart = op.bi
				}
				aLen++
				bLen++
			case '-':
				if aStart < 0 {
					aStart = op.ai
				}
				aLen++
				d.Removed++
			case '+':
				if bStart < 0 {
					bStart = op.bi
				}
				bLen++
				d.Added++
			}
			body.WriteByte(op.kind)
			body.WriteString(strings.TrimSuffix(op.text, "\n"))
			body.WriteByte('\n')
			if !strings.HasSuffix(op.text, "\n") {
				body.WriteString(noNewlineMarker + "\n")
			}
		}
		if aStart < 0 {
			aStart = hunkAnchor(ops, start, true)
		}
		if bStart < 0 {
			bStart = hunkAnchor(ops, start, false)
		}
		// An empty side names the line it follows ("-0,0" for a new file).
		aNum, bNum := aStart+1, bStart+1
		if aLen == 0 {
			aNum = aStart
		}
		if bLen == 0 {
			bNum = bStart
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aNum, aLen, bNum, bLen)
		sb.WriteString(body.String())
		i = stop
	}
	d.Text = strings.TrimSuffix(sb.String(), "\n")
	return d
}

// hunkAnchor is the line a hunk with no line on one side starts after: the
// position of the nearest earlier line of that side.
func hunkAnchor(ops []diffOp, from int, old bool) int {
	for k := from - 1; k >= 0; k-- {
		if old && ops[k].kind != '+' {
			return ops[k].ai + 1
		}
		if !old && ops[k].kind != '-' {
			return ops[k].bi + 1
		}
	}
	return 0
}

// splitDiffLines splits s into lines that keep their "\n", so a last line
// without one compares unequal to the same text with one, as in diff -u. A
// CRLF ending is compared as "\n" (a converted file would otherwise differ on
// every line) and recorded in crlf instead, for lineEndingNote.
//
// It used to normalise CRLF and trim the final newline before splitting, so
// "a\n" → "a", or a file converted between LF and CRLF, gave an empty diff and
// the permission prompt reported the write as "内容不变".
func splitDiffLines(s string) (lines []string, crlf []bool) {
	for len(s) > 0 {
		n := strings.IndexByte(s, '\n')
		if n < 0 {
			lines = append(lines, s)
			crlf = append(crlf, false)
			break
		}
		line := s[:n+1]
		s = s[n+1:]
		cr := strings.HasSuffix(line, "\r\n")
		if cr {
			line = line[:len(line)-2] + "\n"
		}
		lines = append(lines, line)
		crlf = append(crlf, cr)
	}
	return lines, crlf
}

// noNewlineMarker follows a line that ends the text without a newline.
const noNewlineMarker = `\ No newline at end of file`

// lineEndingNote describes a change of line endings that the diff's lines do
// not show ("" when there is none): the lines are compared with CRLF read as
// LF, so a line whose only change is its ending is context. The note is
// written when such a context line exists or when the files' overall styles
// differ.
func lineEndingNote(ops []diffOp, a, b []string, crA, crB []bool) string {
	changed := 0
	for _, op := range ops {
		if op.kind == ' ' && crA[op.ai] != crB[op.bi] {
			changed++
		}
	}
	oldStyle, newStyle := lineEndingStyle(a, crA), lineEndingStyle(b, crB)
	if oldStyle == "" || newStyle == "" || (changed == 0 && oldStyle == newStyle) {
		return ""
	}
	if changed == 0 {
		return fmt.Sprintf(`\ Line endings changed: %s → %s`, oldStyle, newStyle)
	}
	return fmt.Sprintf(`\ Line endings changed on %d unchanged line(s): %s → %s`, changed, oldStyle, newStyle)
}

// lineEndingStyle is "CRLF", "LF" or "mixed CRLF/LF" for the terminated lines,
// "" when no line ends in a newline.
func lineEndingStyle(lines []string, crlf []bool) string {
	var cr, lf int
	for i, l := range lines {
		switch {
		case !strings.HasSuffix(l, "\n"):
		case crlf[i]:
			cr++
		default:
			lf++
		}
	}
	switch {
	case cr == 0 && lf == 0:
		return ""
	case lf == 0:
		return "CRLF"
	case cr == 0:
		return "LF"
	}
	return "mixed CRLF/LF"
}

// ColorDiff colours a diff for the terminal: additions green, removals red,
// hunk headers cyan. plain returns it unchanged (NO_COLOR, a pipe).
func ColorDiff(diff string, plain bool) string {
	if plain {
		return diff
	}
	lines := strings.Split(diff, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "@@"):
			lines[i] = "\x1b[36m" + l + "\x1b[0m"
		case strings.HasPrefix(l, "+"):
			lines[i] = "\x1b[32m" + l + "\x1b[0m"
		case strings.HasPrefix(l, "-"):
			lines[i] = "\x1b[31m" + l + "\x1b[0m"
		}
	}
	return strings.Join(lines, "\n")
}
