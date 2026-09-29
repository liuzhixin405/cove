package render

import (
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// Code blocks in answers are coloured line by line: keywords, strings,
// comments and numbers. It is a lexical pass, not a parser — a line is all
// it ever sees (a block comment or a multi-line string spanning lines is
// coloured as code after its first line), which is what a stream allows. A
// block whose language is not known is left plain.

const (
	sgrKeyword = "\x1b[35m"
	sgrString  = "\x1b[32m"
	sgrComment = "\x1b[90m"
	sgrNumber  = "\x1b[33m"
	sgrFgOff   = "\x1b[39m"
)

type codeLang struct {
	keywords     map[string]bool
	lineComments []string
	quotes       string // characters that open a string
}

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

var (
	langGo = &codeLang{keywords: words("break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false"),
		lineComments: []string{"//"}, quotes: "\"'`"}
	langPy = &codeLang{keywords: words("and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield None True False self"),
		lineComments: []string{"#"}, quotes: "\"'"}
	langJS = &codeLang{keywords: words("async await break case catch class const continue default delete do else export extends finally for from function if import in instanceof interface let new of return static super switch this throw try type typeof var void while yield null undefined true false enum implements private protected public readonly"),
		lineComments: []string{"//"}, quotes: "\"'`"}
	langCS = &codeLang{keywords: words("abstract as async await base bool break byte case catch char class const continue decimal default delegate do double else enum event explicit extern false finally fixed float for foreach get if implicit in int interface internal is lock long namespace new null object operator out override params private protected public readonly record ref return sealed set short static string struct switch this throw true try typeof uint ulong using var virtual void volatile while"),
		lineComments: []string{"//"}, quotes: "\"'"}
	langJava = &codeLang{keywords: words("abstract boolean break byte case catch char class const continue default do double else enum extends final finally float for if implements import instanceof int interface long native new null package private protected public return short static super switch synchronized this throw throws try void volatile while true false var record"),
		lineComments: []string{"//"}, quotes: "\"'"}
	langRust = &codeLang{keywords: words("as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while"),
		lineComments: []string{"//"}, quotes: "\""}
	langSh = &codeLang{keywords: words("if then else elif fi for in do done while until case esac function return export local set echo cd"),
		lineComments: []string{"#"}, quotes: "\"'"}
	langSQL = &codeLang{keywords: words("select from where insert into values update set delete create table alter drop index join left right inner outer on group by order having limit and or not null as distinct union primary key SELECT FROM WHERE INSERT INTO VALUES UPDATE SET DELETE CREATE TABLE ALTER DROP INDEX JOIN LEFT RIGHT INNER OUTER ON GROUP BY ORDER HAVING LIMIT AND OR NOT NULL AS DISTINCT UNION PRIMARY KEY"),
		lineComments: []string{"--"}, quotes: "'\""}
	langJSON = &codeLang{keywords: words("true false null"), quotes: "\""}
)

var langByName = map[string]*codeLang{
	"go": langGo, "golang": langGo,
	"py": langPy, "python": langPy,
	"js": langJS, "javascript": langJS, "ts": langJS, "typescript": langJS, "tsx": langJS, "jsx": langJS,
	"cs": langCS, "csharp": langCS, "c#": langCS,
	"java": langJava, "kotlin": langJava, "kt": langJava,
	"rust": langRust, "rs": langRust,
	"sh": langSh, "bash": langSh, "shell": langSh, "zsh": langSh, "powershell": langSh, "ps1": langSh,
	"sql":  langSQL,
	"json": langJSON, "jsonc": langJSON,
	"c": langJava, "cpp": langJava, "c++": langJava,
}

// highlightCode colours one line of a code block in lang; unknown languages
// come back unchanged.
func highlightCode(line, lang string) string {
	l := langByName[lang]
	if l == nil || line == "" {
		return line
	}
	var sb strings.Builder
	i := 0
	for i < len(line) {
		c := line[i]
		// Comment: the rest of the line.
		for _, lc := range l.lineComments {
			if strings.HasPrefix(line[i:], lc) {
				sb.WriteString(sgrComment + line[i:] + sgrFgOff)
				return sb.String()
			}
		}
		switch {
		case strings.IndexByte(l.quotes, c) >= 0:
			j := i + 1
			for j < len(line) && line[j] != c {
				if line[j] == '\\' && c != '`' {
					j++
				}
				j++
			}
			if j < len(line) {
				j++
			}
			j = min(j, len(line))
			sb.WriteString(sgrString + line[i:j] + sgrFgOff)
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(line) && isIdentPart(line[j]) {
				j++
			}
			w := line[i:j]
			if l.keywords[w] {
				sb.WriteString(sgrKeyword + w + sgrFgOff)
			} else {
				sb.WriteString(w)
			}
			i = j
		case c >= '0' && c <= '9' && (i == 0 || !isIdentPart(line[i-1])):
			j := i + 1
			for j < len(line) && (isIdentPart(line[j]) || line[j] == '.') {
				j++
			}
			sb.WriteString(sgrNumber + line[i:j] + sgrFgOff)
			i = j
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return sb.String()
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// renderTable lays out the rows of a Markdown table with aligned columns and
// box-drawing rules; the row after a "---" separator row is the body, the
// rows before it the (bold) header.
func renderTable(rows []string) string {
	var cells [][]string
	header := -1
	for _, r := range rows {
		r = strings.TrimSpace(r)
		r = strings.TrimPrefix(r, "|")
		r = strings.TrimSuffix(r, "|")
		parts := strings.Split(r, "|")
		sep := true
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
			if strings.Trim(parts[i], ":-") != "" || parts[i] == "" {
				sep = false
			}
		}
		if sep && header < 0 {
			header = len(cells)
			continue
		}
		cells = append(cells, parts)
	}
	cols := 0
	for _, r := range cells {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return ""
	}
	width := make([]int, cols)
	for _, r := range cells {
		for i, c := range r {
			width[i] = max(width[i], textutil.Width(c))
		}
	}
	rule := func(l, m, r string) string {
		var sb strings.Builder
		sb.WriteString(codeGutter + sgrDim + l)
		for i, w := range width {
			if i > 0 {
				sb.WriteString(m)
			}
			sb.WriteString(strings.Repeat("─", w+2))
		}
		sb.WriteString(r + sgrDimOff + "\n")
		return sb.String()
	}
	var sb strings.Builder
	sb.WriteString(rule("┌", "┬", "┐"))
	for ri, r := range cells {
		sb.WriteString(codeGutter + sgrDim + "│" + sgrDimOff)
		for i := 0; i < cols; i++ {
			c := ""
			if i < len(r) {
				c = r[i]
			}
			pad := strings.Repeat(" ", width[i]-textutil.Width(c))
			if ri < header {
				c = sgrBold + c + sgrBoldOff
			}
			sb.WriteString(" " + c + pad + " " + sgrDim + "│" + sgrDimOff)
		}
		sb.WriteString("\n")
		if ri == header-1 {
			sb.WriteString(rule("├", "┼", "┤"))
		}
	}
	sb.WriteString(rule("└", "┴", "┘"))
	return sb.String()
}
