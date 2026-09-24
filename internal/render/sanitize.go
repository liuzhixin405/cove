package render

import (
	"strings"
	"unicode/utf8"
)

// Terminal-control sanitising.
//
// Everything cove shows that did not come from cove itself is untrusted: the
// model's reply, its reasoning, a command it wants to run, the first line of a
// file or a web page that ends up in a tool summary, a shell command's live
// output. That text used to reach the terminal verbatim, so an escape sequence
// inside it was executed rather than shown. A README could retitle the window
// (OSC 0), write to the clipboard (OSC 52), move the cursor up and repaint rows
// cove had already printed, clear the screen, or ask the terminal for its
// cursor position (CSI 6n) — which the terminal answers by typing into cove's
// own stdin. In the permission box it was worse: "\r" or a cursor move let the
// text shown for approval differ from the command that would run.
//
// Two policies, because two kinds of text pass through:
//
//   - StripControls is for a single logical line the renderer styles itself
//     (a header, a summary, a prompt description). Nothing survives but text,
//     newlines and tabs.
//   - SanitizeStream is for output that legitimately carries its own styling
//     (go test, git and npm colour their output; progress bars redraw their
//     row). SGR colour, erase-in-line and "\r" are kept, because none of them
//     can reach outside the row the cursor is already on. Everything else is
//     dropped.

// StripControls removes every escape sequence and control character from s
// except '\n' and '\t'.
func StripControls(s string) string {
	out, rest := scanControls(s, false)
	_ = rest // an incomplete trailing sequence is dropped
	return out
}

// SanitizeStream removes every escape sequence and control character from s
// except SGR colour, erase-in-line, '\n', '\r' and '\t'. An incomplete
// sequence at the end of s is dropped; use a StreamSanitizer when s is one
// chunk of a longer stream.
func SanitizeStream(s string) string {
	out, _ := scanControls(s, true)
	return out
}

// maxPendingSequence bounds how much of an unterminated sequence a
// StreamSanitizer holds back waiting for the next chunk. A colour code is a
// handful of bytes; anything longer than this is not one, and holding it would
// let an unterminated OSC swallow the rest of the stream.
const maxPendingSequence = 256

// StreamSanitizer applies SanitizeStream to a stream delivered in chunks.
//
// Chunks are cut wherever the transport cut them, so a colour code can
// straddle two of them. Sanitising each chunk on its own would drop the first
// half and print the second ("1m") as text; this holds an incomplete trailing
// sequence back and prepends it to the next chunk. The zero value is ready to
// use. It is not safe for concurrent use.
type StreamSanitizer struct {
	pending string
}

// Write sanitises chunk and returns the text that is safe to print now.
func (z *StreamSanitizer) Write(chunk string) string {
	s := z.pending + chunk
	z.pending = ""
	var sb strings.Builder
	for {
		out, rest := scanControls(s, true)
		sb.WriteString(out)
		if rest == "" {
			break
		}
		if len(rest) <= maxPendingSequence {
			z.pending = rest
			break
		}
		// Too long to be a sequence worth keeping: drop its introducer and
		// let what follows print as the plain text it now is.
		s = rest[min(2, len(rest)):]
	}
	return sb.String()
}

// scanControls filters s. keepStyle selects the SanitizeStream policy. rest is
// the suffix of s starting at an escape sequence that s ends before
// completing; it is not part of out.
func scanControls(s string, keepStyle bool) (out, rest string) {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		b := s[i]
		switch {
		case b == 0x1b:
			end, complete, keep := escapeSequence(s, i)
			if !complete {
				return sb.String(), s[i:]
			}
			if keep && keepStyle {
				sb.WriteString(s[i:end])
			}
			i = end
		case b == '\n' || b == '\t':
			sb.WriteByte(b)
			i++
		case b == '\r':
			if keepStyle {
				sb.WriteByte(b)
			}
			i++
		case b < 0x20 || b == 0x7f:
			i++
		case b < utf8.RuneSelf:
			sb.WriteByte(b)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				// A stray byte in 0x80–0x9F is an 8-bit C1 control to a
				// terminal that accepts them; anything else is left for the
				// terminal to show as a replacement glyph.
				if b > 0x9f {
					sb.WriteByte(b)
				}
			case r >= 0x80 && r <= 0x9f:
				// C1 controls, including the one-character CSI (U+009B) and
				// OSC (U+009D) introducers. Dropping the introducer is enough:
				// what follows it is inert text.
			default:
				sb.WriteString(s[i : i+size])
			}
			i += size
		}
	}
	return sb.String(), ""
}

// escapeSequence parses the escape sequence starting at s[i] (an ESC). end is
// the index just past it; complete is false when s ends first; keep reports
// whether it is one of the row-local sequences SanitizeStream allows.
func escapeSequence(s string, i int) (end int, complete, keep bool) {
	if i+1 >= len(s) {
		return len(s), false, false
	}
	switch next := s[i+1]; {
	case next == '[':
		// CSI: parameter bytes, intermediate bytes, one final byte.
		j := i + 2
		paramsStart := j
		for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
			j++
		}
		params := s[paramsStart:j]
		interStart := j
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j >= len(s) {
			return len(s), false, false
		}
		final := s[j]
		if final < 0x40 || final > 0x7e {
			// Malformed: drop what was parsed and resume at the odd byte.
			return j, true, false
		}
		plain := j == interStart && strings.Trim(params, "0123456789;:") == ""
		return j + 1, true, plain && (final == 'm' || final == 'K')
	case next == ']' || next == 'P' || next == 'X' || next == '^' || next == '_':
		// OSC, DCS, SOS, PM, APC: a string ended by BEL or ST (ESC \).
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1, true, false
			}
			if s[j] == 0x1b {
				if j+1 >= len(s) {
					return len(s), false, false
				}
				if s[j+1] == '\\' {
					return j + 2, true, false
				}
			}
		}
		return len(s), false, false
	case next >= 0x20 && next <= 0x2f:
		// nF sequences such as a charset designation (ESC ( 0).
		j := i + 1
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j >= len(s) {
			return len(s), false, false
		}
		if s[j] >= 0x30 && s[j] <= 0x7e {
			return j + 1, true, false
		}
		return j, true, false
	case next >= 0x30 && next <= 0x7e:
		// Two-byte sequences: ESC 7 (save cursor), ESC c (full reset), ...
		return i + 2, true, false
	default:
		// ESC followed by a control or non-ASCII byte: drop the ESC alone.
		return i + 1, true, false
	}
}
