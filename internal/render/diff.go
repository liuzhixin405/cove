package render

import (
	"fmt"
	"strings"
)

// LineDiff is a unified line diff of two texts, for showing what an edit or
// a write did (the tool block's expanded form, the permission preview).
type LineDiff struct {
	// Text is the diff: "@@ -a,b +c,d @@" hunks with 3 lines of context,
	// "-" and "+" lines. Empty when the texts are equal.
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
	a, b := splitDiffLines(old), splitDiffLines(newText)

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
	return formatHunks(ops)
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
				if aStart < 0 {
					aStart, bStart = op.ai, op.bi
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
			body.WriteString(op.text)
			body.WriteByte('\n')
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

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
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
