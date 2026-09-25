package render

import (
	"strings"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// Layout constants.
//
// A single gutter width for every block is the whole point: the previous code
// had the user line at column 0, tool lines at column 2, and thinking at column
// 2 with a different marker, so nothing lined up vertically and the transcript
// read as noise. Every block now starts its content at the same column.
const (
	// gutter is the indent applied to every block's content.
	gutter = "  "
	// contIndent is where a continuation ("⎿") line starts, one step inside the
	// gutter so the hierarchy is visible without a box or a border.
	contIndent = "    "
	// minRenderWidth is the narrowest terminal the layout still tries to serve.
	minRenderWidth = 20
)

// Glyphs are the gutter symbols.
//
// # Disclosure is the organising idea
//
// Closed/Open/Leaf are one column that answers a single question: can this row
// be opened, and is it open? Everything expandable gets the triangle, whether
// it is a tool call or a step of reasoning, and everything that has nothing
// hidden gets a blank.
//
// The previous set failed that. Reasoning got "▸" — which every UI on earth
// uses to mean "click to open" — while a tool call, the thing whose output you
// most want to see, got a decorative "⚙" that promises nothing. So the row with
// hidden output looked inert and the row with almost nothing in it looked
// clickable, which is backwards.
//
// # Why the set is swappable
//
// The state glyphs carry meaning, so they must differ per state: engine's old
// formatToolLine printed a literal "?" for BOTH success and failure, leaving
// colour as the only signal. That "?" was mojibake — the glyph mangled by a
// non-UTF-8 console — which is why there is an ASCII set rather than a
// hardcoded one.
type Glyphs struct {
	// User marks what the user typed.
	User string
	// Closed and Open are the disclosure marker for an expandable block.
	// This package only prints Closed (it no longer renders an expanded
	// form); Open stays so front ends that do can share the glyph set.
	Closed string
	Open   string
	// Leaf occupies the disclosure column for a block with nothing hidden, so
	// its text still lines up with its neighbours.
	Leaf string
	// OK and Err are the outcome of a tool call.
	OK  string
	Err string
	// Cont starts the summary line under a header.
	Cont string
}

// UnicodeGlyphs is the default set.
func UnicodeGlyphs() Glyphs {
	return Glyphs{
		User:   "›",
		Closed: "▸",
		Open:   "▾",
		Leaf:   " ",
		OK:     "✓",
		Err:    "✗",
		Cont:   "⎿",
	}
}

// ASCIIGlyphs is the fallback for a console that cannot render the above.
//
// "+"/"-" for disclosure is the convention in ASCII tree views, and the
// success/failure pair stays asymmetric ("ok" vs "ERR") because telling those
// two apart at a glance is the one thing the gutter has to do.
func ASCIIGlyphs() Glyphs {
	return Glyphs{
		User:   ">",
		Closed: "+",
		Open:   "-",
		Leaf:   " ",
		OK:     "ok",
		Err:    "ERR",
		Cont:   "|-",
	}
}

// glyphs resolves the set to use. An unset Glyphs means Unicode, so the zero
// Styles value stays a usable plain-text renderer.
func (s Styles) glyphs() Glyphs {
	if s.Glyphs.Cont == "" {
		return UnicodeGlyphs()
	}
	return s.Glyphs
}

// disclosure returns the marker for b's disclosure column.
func (s Styles) disclosure(b Block) string {
	g := s.glyphs()
	if !b.Expandable() {
		return g.Leaf
	}
	return g.Closed
}

// Styles carries the styling functions the renderer applies. Passing them in
// keeps this package free of any dependency on a theme or on terminal
// detection, so the layout can be asserted in a test with Styles{} (which
// renders plain text).
//
// A nil field means "no styling", so the zero value is a usable plain-text
// renderer.
type Styles struct {
	User     func(string) string
	Thinking func(string) string
	ToolName func(string) string
	Summary  func(string) string
	OK       func(string) string
	Err      func(string) string
	Hint     func(string) string
	Answer   func(string) string

	// Glyphs selects the gutter symbol set. Leave it unset for Unicode; a
	// front end on a console that cannot render those passes ASCIIGlyphs.
	Glyphs Glyphs
}

func apply(f func(string) string, s string) string {
	if f == nil {
		return s
	}
	return f(s)
}

// Collapsed renders the default, folded form of a block: one header line plus
// at most one "⎿" summary line, and an expand hint only when there is actually
// something hidden.
//
// width is the terminal width. Every returned line is clipped to it, because
// the caller prints this straight into the terminal where a line one column too
// wide soft-wraps and silently adds a row.
func Collapsed(b Block, width int, st Styles) string {
	if width < minRenderWidth {
		width = minRenderWidth
	}
	b = withSafeText(b)

	g := st.glyphs()

	switch b.Kind {
	case KindUser:
		// The user's own text is never folded or clipped: they wrote it, and
		// hiding part of it is disorienting.
		return wrapBlock(apply(st.User, g.User+" ")+b.Header, width, gutter, textutil.Width(g.User)+1)

	case KindAnswer:
		return wrapBlock(apply(st.Answer, b.Header), width, gutter, 0)

	case KindSystem:
		style := st.Summary
		glyph := " "
		if b.IsError {
			style = st.Err
			glyph = g.Err
		}
		return wrapBlock(apply(style, glyph+" "+b.Header), width, gutter, textutil.Width(glyph)+1)

	case KindThinking:
		label := st.disclosure(b) + " 思考"
		if b.Header != "" {
			label += " " + b.Header
		}
		return clipLine(gutter+apply(st.Thinking, label), width)

	case KindTool:
		return collapsedTool(b, width, st)
	}
	return ""
}

// collapsedTool renders the two-line folded form of a tool call:
//
//	⚙ bash   go test ./internal/tool/
//	  ⎿ ok 0.336s · 共 12 行
func collapsedTool(b Block, width int, st Styles) string {
	var sb strings.Builder

	// Header: disclosure marker + tool name + target.
	name := b.Tool
	if name == "" {
		name = "tool"
	}
	g := st.glyphs()
	// The marker is styled with the name, not dimmed: it is the affordance,
	// and chrome you have to hunt for is not an affordance.
	head := gutter + apply(st.ToolName, st.disclosure(b)+" "+name)
	if b.Header != "" {
		head += " " + b.Header
	}
	sb.WriteString(clipLine(head, width))

	// Summary line, with the status glyph so success and failure are
	// distinguishable without relying on color.
	if b.Summary != "" {
		status, style := g.OK, st.Summary
		if b.IsError {
			status, style = g.Err, st.Err
		}
		cont := contIndent + apply(style, g.Cont+" "+status+" "+b.Summary)
		sb.WriteString("\n")
		sb.WriteString(clipLine(cont, width))
	} else {
		return clipLine(head, width)
	}
	return sb.String()
}

// withSafeText neutralises terminal controls in the block's text fields.
//
// They are untrusted - the model's command, a file's first line, a web page -
// and used to be printed verbatim, so an escape sequence in them was executed
// by the terminal (see sanitize.go). The single-line fields lose every
// control; the full output keeps the tool's own colour, which is the only
// styling a reader of a build log relies on.
func withSafeText(b Block) Block {
	b.Header = StripControls(b.Header)
	b.Summary = StripControls(b.Summary)
	b.FullPath = StripControls(b.FullPath)
	b.Full = SanitizeStream(b.Full)
	return b
}

// clipLine clips one line to width without splitting an escape sequence or a
// rune. ansi.Truncate is both escape- and grapheme-aware.
func clipLine(s string, width int) string {
	if textutil.Width(s) <= width {
		return s
	}
	return textutil.TruncateWidth(s, width, "…")
}

// wrapBlock hard-wraps text to width, indenting every line with indent and
// continuation lines by an extra hang columns so wrapped prose stays aligned
// under its first line.
func wrapBlock(text string, width int, indent string, hang int) string {
	avail := width - textutil.Width(indent)
	if avail < 8 {
		return clipLine(indent+oneLine(text), width)
	}
	hangPad := strings.Repeat(" ", hang)

	var out []string
	for _, para := range strings.Split(text, "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		first := true
		for len(para) > 0 {
			budget := avail
			if !first {
				budget = avail - hang
			}
			if budget < 8 {
				budget = 8
			}
			if textutil.Width(para) <= budget {
				line := indent + para
				if !first {
					line = indent + hangPad + para
				}
				out = append(out, line)
				break
			}
			cut := breakAt(para, budget)
			line := indent + strings.TrimRight(para[:cut], " ")
			if !first {
				line = indent + hangPad + strings.TrimRight(para[:cut], " ")
			}
			out = append(out, line)
			para = strings.TrimLeft(para[cut:], " ")
			first = false
		}
	}
	return strings.Join(out, "\n")
}

// breakAt returns the byte offset to wrap at: the last space within the width
// budget, or the budget itself (on a rune boundary) when the run has no space —
// which is the normal case for Chinese text, where there are no word breaks.
func breakAt(s string, budget int) int {
	limit := len(s)
	if w := textutil.Width(s); w > budget {
		limit = len(textutil.HeadRunes(s, budget))
		// HeadRunes counts runes, but budget is display columns; CJK runes are
		// two columns wide, so walk back until the prefix fits.
		for limit > 0 && textutil.Width(s[:limit]) > budget {
			limit = len(textutil.HeadRunes(s[:limit], utf8RuneCount(s[:limit])-1))
		}
	}
	if i := strings.LastIndexByte(s[:limit], ' '); i > budget/2 {
		return i
	}
	if limit == 0 {
		return len(s)
	}
	return limit
}

func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
