package api

import "strings"

// escapeUnescapedQuotes rewrites raw so that a double quote inside a JSON
// string value that cannot be its closing quote is escaped. A quote closes
// a string only when the next non-space character continues the JSON
// structure (, } ] or :); a quote followed by anything else (a letter, a
// space then a letter, an operator) is text the model forgot to escape:
//
//	{"command":"cd "D:/github/agent" && dotnet new sln"}
//
// Valid JSON is returned unchanged. Only the string-value scanner changes;
// truncated input still ends inside a string and still fails to parse.
func escapeUnescapedQuotes(raw string) string {
	var sb strings.Builder
	sb.Grow(len(raw) + 8)
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			sb.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			// An escape sequence: copy both bytes untouched.
			sb.WriteByte(c)
			if i+1 < len(raw) {
				i++
				sb.WriteByte(raw[i])
			}
		case '"':
			if closesString(raw, i+1) {
				inString = false
				sb.WriteByte(c)
			} else {
				sb.WriteString(`\"`)
			}
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// closesString reports whether a quote at position i-1 of raw is followed,
// after optional whitespace, by a character that continues the JSON
// structure. End of input counts as closing, so truncation is not "fixed".
func closesString(raw string, i int) bool {
	for ; i < len(raw); i++ {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case ',', '}', ']', ':':
			return true
		default:
			return false
		}
	}
	return true
}
