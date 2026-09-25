package render

import (
	"strings"

	"github.com/liuzhixin405/cove/internal/textmode"
)

// Streaming Markdown for the model's answer.
//
// The answer arrives as deltas cut wherever the transport cut them, and it has
// to appear as it arrives: waiting for a whole paragraph (or a whole line of
// Chinese prose, which has no line breaks for a long time) would make the
// reply look stalled. So this is not a Markdown parser but a line-oriented
// state machine that decides how to show each line from its first few bytes
// and then streams the rest of the line through:
//
//   - "# " … "###### " headings: the markers are dropped and the line is bold.
//   - "- ", "* ", "+ " bullets: the marker becomes "•".
//   - "> " quotes: a dim bar.
//   - ``` / ~~~ fences: the fence lines become a dim border and the code lines
//     are indented behind a dim bar; nothing inside is formatted. Whether a
//     code block is open is kept across lines and chunks.
//   - Inline, "**" toggles bold and "`" toggles reverse video. Every style is
//     reset at the end of its line, so an unmatched marker cannot leak.
//
// Only a possible marker is ever held back — the undecided start of a line
// ("#", "``", "-"), or a trailing "*" that may be the first half of "**" —
// so the output is the same however the input is chunked. A parser such as
// goldmark was considered and not used: it needs the whole block before it
// can say what it is, which is exactly what streaming cannot give it.
//
// The input must already be sanitised (see StreamSanitizer); the styling
// added here is plain SGR.

const (
	sgrBold    = "\x1b[1m"
	sgrBoldOff = "\x1b[22m"
	sgrDim     = "\x1b[2m"
	sgrDimOff  = "\x1b[22m"
	sgrReverse = "\x1b[7m"
	sgrRevOff  = "\x1b[27m"
	sgrReset   = "\x1b[0m"
	codeGutter = "  "
)

type mdGlyphs struct {
	bullet, bar, fenceOpen, fenceClose string
}

var (
	mdUnicode = mdGlyphs{bullet: "•", bar: "│", fenceOpen: "┌─", fenceClose: "└─"}
	mdASCII   = mdGlyphs{bullet: "-", bar: "|", fenceOpen: "+-", fenceClose: "+-"}
)

type mdLine int

const (
	mdText    mdLine = iota // paragraph, bullet or quote content: inline styles apply
	mdHeading               // bold line; "**" markers are dropped
	mdCode                  // inside a fence: printed verbatim
)

// MarkdownStream renders a Markdown stream chunk by chunk. It is not safe for
// concurrent use.
type MarkdownStream struct {
	g mdGlyphs

	// Code-fence state, kept across lines: fenceLen > 0 means a block is open.
	fenceChar byte
	fenceLen  int

	// pending is input held back until the next chunk decides what it is.
	pending string

	// Current-line state.
	started    bool // the line's prefix has been decided and printed
	kind       mdLine
	bold, code bool
}

// NewMarkdownStream returns a renderer that uses ASCII glyphs when
// textmode.PreferASCII says the console cannot show the Unicode ones.
func NewMarkdownStream() *MarkdownStream {
	m := &MarkdownStream{g: mdUnicode}
	if textmode.PreferASCII() {
		m.g = mdASCII
	}
	return m
}

// Write renders chunk and returns what can be printed now.
func (m *MarkdownStream) Write(chunk string) string {
	return m.render(chunk, false)
}

// Flush prints whatever is held back and closes the current line's styles.
// Call it when the stream ends or another output source is about to print
// (a tool block interrupting the answer). The text that follows starts a new
// line as far as Markdown is concerned; an open code block stays open.
func (m *MarkdownStream) Flush() string {
	out := m.render("", true)
	if m.bold || m.code || (m.started && m.kind == mdHeading) {
		out += sgrReset
	}
	m.started, m.bold, m.code = false, false, false
	return out
}

func (m *MarkdownStream) render(chunk string, final bool) string {
	s := m.pending + chunk
	m.pending = ""
	var out strings.Builder
	for len(s) > 0 {
		if !m.started {
			line, hasNL := s, false
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				line, hasNL = s[:i], true
			}
			consumed, ok := m.startLine(&out, line, hasNL, hasNL || final)
			if !ok {
				m.pending = s
				break
			}
			s = s[consumed:]
			continue
		}
		s = m.inline(&out, s, final)
	}
	return out.String()
}

// startLine decides how the line beginning at line is shown and prints its
// prefix. complete says the whole line is known (a newline follows, or the
// stream is being flushed). It returns how much input it consumed, or false
// when more input is needed to decide. A fence line is consumed whole,
// newline included; for every other line it leaves m.started set.
func (m *MarkdownStream) startLine(out *strings.Builder, line string, hasNL, complete bool) (int, bool) {
	indent := len(line) - len(strings.TrimLeft(line, " \t"))
	u := line[indent:]
	wholeLine := len(line)
	if hasNL {
		wholeLine++
	}
	nl := ""
	if hasNL {
		nl = "\n"
	}

	if m.fenceLen > 0 {
		if u == "" && !complete {
			return 0, false
		}
		if u != "" && u[0] == m.fenceChar {
			r := runOf(u, m.fenceChar)
			switch {
			case r == len(u) && !complete:
				return 0, false // "``" may still become the closing "```"
			case r >= m.fenceLen && strings.TrimSpace(u[r:]) == "":
				if !complete {
					return 0, false
				}
				out.WriteString(codeGutter + sgrDim + m.g.fenceClose + sgrDimOff + nl)
				m.fenceLen = 0
				return wholeLine, true
			}
		}
		out.WriteString(codeGutter + sgrDim + m.g.bar + sgrDimOff + " ")
		m.begin(mdCode)
		return 0, true
	}

	if u == "" {
		if !complete {
			return 0, false
		}
		m.begin(mdText)
		return 0, true
	}
	switch c := u[0]; c {
	case '#':
		n := runOf(u, '#')
		if n > 6 {
			break
		}
		if n == len(u) {
			if !complete {
				return 0, false
			}
			out.WriteString(sgrBold)
			m.begin(mdHeading)
			return len(line), true
		}
		if u[n] == ' ' || u[n] == '\t' {
			out.WriteString(sgrBold)
			m.begin(mdHeading)
			return indent + n + 1, true
		}
	case '-', '*', '+':
		if len(u) == 1 {
			if !complete {
				return 0, false
			}
			break
		}
		if u[1] == ' ' || u[1] == '\t' {
			out.WriteString(line[:indent] + m.g.bullet + " ")
			m.begin(mdText)
			return indent + 2, true
		}
	case '`', '~':
		r := runOf(u, c)
		if r < 3 {
			if r == len(u) && !complete {
				return 0, false
			}
			break
		}
		if !complete {
			return 0, false // the info string runs to the end of the line
		}
		info := strings.TrimSpace(u[r:])
		if c == '`' && strings.ContainsRune(info, '`') {
			break // not a fence (CommonMark): inline code at the start of a line
		}
		label := m.g.fenceOpen
		if info != "" {
			label += " " + StripControls(info)
		}
		out.WriteString(codeGutter + sgrDim + label + sgrDimOff + nl)
		m.fenceChar, m.fenceLen = c, r
		return wholeLine, true
	case '>':
		skip := indent + 1
		if len(u) > 1 && (u[1] == ' ' || u[1] == '\t') {
			skip++
		} else if len(u) == 1 && !complete {
			return 0, false
		}
		out.WriteString(line[:indent] + sgrDim + m.g.bar + sgrDimOff + " ")
		m.begin(mdText)
		return skip, true
	}
	m.begin(mdText)
	return 0, true
}

func (m *MarkdownStream) begin(k mdLine) {
	m.started, m.kind, m.bold, m.code = true, k, false, false
}

// inline prints the rest of the current line from s and returns the input
// after it (after its newline, when s holds one).
func (m *MarkdownStream) inline(out *strings.Builder, s string, final bool) string {
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\n' {
			m.endLine(out)
			out.WriteByte('\n')
			return s[i+1:]
		}
		if m.kind == mdCode {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				out.WriteString(s[i:])
				return ""
			}
			out.WriteString(s[i : i+j])
			i += j
			continue
		}
		switch c {
		case '*':
			if i+1 >= len(s) {
				if !final {
					m.pending = "*" // may be the first half of "**"
					return ""
				}
				out.WriteByte('*')
				i++
				continue
			}
			if s[i+1] == '*' && !m.code {
				if m.kind != mdHeading {
					m.bold = !m.bold
					if m.bold {
						out.WriteString(sgrBold)
					} else {
						out.WriteString(sgrBoldOff)
					}
				}
				i += 2
				continue
			}
			out.WriteByte('*')
			i++
		case '`':
			m.code = !m.code
			if m.code {
				out.WriteString(sgrReverse)
			} else {
				out.WriteString(sgrRevOff)
			}
			i++
		default:
			j := i + 1
			for j < len(s) && s[j] != '\n' && s[j] != '*' && s[j] != '`' {
				j++
			}
			out.WriteString(s[i:j])
			i = j
		}
	}
	return ""
}

// endLine closes the line's styles before its newline.
func (m *MarkdownStream) endLine(out *strings.Builder) {
	if m.bold || m.code || m.kind == mdHeading {
		out.WriteString(sgrReset)
	}
	m.started, m.bold, m.code = false, false, false
}

func runOf(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}
