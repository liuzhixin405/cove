package safety

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Character classes the tokenizer and the permission checks share.
//
// PowerShell receives the line through -Command and reads it with its own
// rules: a lone CR, NEL (U+0085), LINE SEPARATOR (U+2028) and PARAGRAPH
// SEPARATOR (U+2029) end a statement like a newline, and FF, VT, NBSP and
// every other Unicode space separate arguments. The tokenizer used to know
// space, tab and CR only (and read a lone CR as a blank), so "ls<CR>Remove-
// Item -Recurse -Force src" was a read-only ls to every check.

// isStatementBreak reports a character that ends a statement for PowerShell
// besides '\n': a CR not followed by LF, NEL, U+2028 and U+2029. next is the
// character after ch (0 at the end).
func isStatementBreak(ch, next rune) bool {
	switch ch {
	case '\r':
		return next != '\n'
	case '\u0085', ' ', ' ':
		return true
	}
	return false
}

// isBlank reports a character that separates words: space, tab, the CR of a
// CRLF, FF, VT and every Unicode space (unicode.IsSpace, category Zs).
func isBlank(ch rune) bool {
	return unicode.IsSpace(ch) || unicode.Is(unicode.Zs, ch)
}

// braceIsGroup reports whether a '{' or '}' that bash reads outside quotes
// opens or closes a command group: it must start a word ("{" followed by a
// blank, "}" followed by a blank or an operator). Anywhere else it is part of
// the word.
func braceIsGroup(ch rune, inWord bool, next rune) bool {
	if inWord {
		return false
	}
	if next == 0 || next == '\n' || isBlank(next) {
		return true
	}
	return ch == '}' && strings.ContainsRune(";&|)", next)
}

// HasHostileCharacters reports whether command holds a character a shell may
// read differently from the tokenizer, or that renders invisibly: a C0 or C1
// control other than tab and newline (a CR only when it does not end a CRLF),
// DEL, a Unicode space other than U+0020, a line or paragraph separator, a
// format character (zero-width space and joiners, bidi overrides, BOM) or
// invalid UTF-8. Such a line is never read-only, auto-approved or covered by
// an allow rule: it always asks.
func HasHostileCharacters(command string) bool {
	for i := 0; i < len(command); {
		r, size := utf8.DecodeRuneInString(command[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			return true
		case r == '\t' || r == '\n' || r == ' ':
		case r == '\r':
			if i+1 >= len(command) || command[i+1] != '\n' {
				return true
			}
		case r < 0x20 || r >= 0x7f && r <= 0x9f:
			return true
		case isBlank(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Cf):
			return true
		}
		i += size
	}
	return false
}

// StripFormatCharacters removes Unicode format characters (category Cf:
// zero-width space and joiners, bidi controls, BOM, soft hyphen). They render
// as nothing, so "r<ZWSP>m -rf /" looks like rm to the person reading it and
// the hard block judges it as that.
func StripFormatCharacters(s string) string {
	if !strings.ContainsFunc(s, isFormat) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isFormat(r) {
			return -1
		}
		return r
	}, s)
}

func isFormat(r rune) bool { return unicode.Is(unicode.Cf, r) }

// maxBraceExpansion caps how many words braceExpand produces.
const maxBraceExpansion = 64

// braceExpand performs bash's comma brace expansion on one word:
// "/{etc,usr}" is "/etc" and "/usr", "{rm,-rf,/}" is three words. A word
// without a {...,...} group is returned as is; sequences ({1..3}) and ${VAR}
// are left alone.
func braceExpand(w string) []string {
	out := []string{w}
	for round := 0; round < 8; round++ {
		var next []string
		changed := false
		for _, s := range out {
			alts, ok := expandFirstBrace(s)
			if !ok {
				next = append(next, s)
				continue
			}
			changed = true
			next = append(next, alts...)
			if len(next) > maxBraceExpansion {
				return next[:maxBraceExpansion]
			}
		}
		out = next
		if !changed {
			break
		}
	}
	return out
}

// expandFirstBrace expands the first {a,b,...} group of s that holds a
// top-level comma.
func expandFirstBrace(s string) ([]string, bool) {
	for start := 0; start < len(s); start++ {
		if s[start] != '{' || start > 0 && s[start-1] == '$' {
			continue
		}
		depth := 0
		var commas []int
		for j := start; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					if len(commas) == 0 {
						j = len(s)
						break
					}
					prefix, suffix := s[:start], s[j+1:]
					var alts []string
					from := start + 1
					for _, c := range append(commas, j) {
						alts = append(alts, prefix+s[from:c]+suffix)
						from = c + 1
					}
					return alts, true
				}
			case ',':
				if depth == 1 {
					commas = append(commas, j)
				}
			}
		}
	}
	return nil, false
}
