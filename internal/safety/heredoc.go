package safety

import "strings"

// Here-document support for parseShell: << and <<- bodies are the lines
// after the command, up to the delimiter, and are stdin data rather than
// commands of the line.

type heredoc struct {
	delim string
	dash  bool // <<- strips leading tabs from body lines and the delimiter line
	body  string
}

// readHeredocBodies consumes the lines of rs after position i (a newline)
// as the bodies of the pending here-documents, in order, and returns the
// index of the last consumed rune. The caller clears pending afterwards.
func readHeredocBodies(rs []rune, i int, pending []*heredoc) int {
	start := i + 1
	for _, h := range pending {
		var body strings.Builder
		for {
			if start >= len(rs) {
				// Unterminated: the body runs to the end of the input.
				h.body = body.String()
				return len(rs) - 1
			}
			end := start
			for end < len(rs) && rs[end] != '\n' {
				end++
			}
			line := strings.TrimSuffix(string(rs[start:end]), "\r")
			start = end + 1
			cmp := line
			if h.dash {
				cmp = strings.TrimLeft(cmp, "\t")
			}
			if cmp == h.delim {
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
		}
		h.body = body.String()
	}
	return start - 1
}

// readHeredocDelimiter reads the here-document delimiter word of rs
// starting at j, with quotes removed, and returns it with the index of its
// last rune.
func readHeredocDelimiter(rs []rune, j int) (string, int) {
	for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
		j++
	}
	var d strings.Builder
	var q rune
	for ; j < len(rs); j++ {
		c := rs[j]
		if q != 0 {
			if c == q {
				q = 0
			} else {
				d.WriteRune(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			q = c
			continue
		}
		if strings.ContainsRune(" \t\r\n;|&<>()", c) {
			break
		}
		d.WriteRune(c)
	}
	// Backslashes quote characters of the delimiter in bash: E\OF is EOF.
	return strings.ReplaceAll(d.String(), `\`, ""), j - 1
}
