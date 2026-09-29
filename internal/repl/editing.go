package repl

// Editing commands beyond insert/delete: word motion, the history search,
// the history file, Esc, and drawing an input that takes several rows.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// isWordRune is what word motion treats as part of a word: letters, digits
// and "_" (a CJK character is a letter).
func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordLeft is the start of the word before cursor.
func wordLeft(buf []rune, cursor int) int {
	i := cursor
	for i > 0 && !isWordRune(buf[i-1]) {
		i--
	}
	for i > 0 && isWordRune(buf[i-1]) {
		i--
	}
	return i
}

// wordRight is the end of the word after cursor.
func wordRight(buf []rune, cursor int) int {
	i := cursor
	for i < len(buf) && !isWordRune(buf[i]) {
		i++
	}
	for i < len(buf) && isWordRune(buf[i]) {
		i++
	}
	return i
}

// deleteWordBack deletes from the start of the word before cursor to cursor.
func deleteWordBack(buf []rune, cursor int) ([]rune, int) {
	start := wordLeft(buf, cursor)
	out := append(append([]rune(nil), buf[:start]...), buf[cursor:]...)
	return out, start
}

// escInterruptEnabled: COVE_ESC_INTERRUPT=0 turns the Esc key off, for a
// connection slow enough to split a key's escape sequence across reads (an
// arrow key would otherwise read as Esc followed by text).
func escInterruptEnabled() bool { return os.Getenv("COVE_ESC_INTERRUPT") != "0" }

// ---------------------------------------------------------------------------
// History search (Ctrl+R)
// ---------------------------------------------------------------------------

// reverseSearch runs the incremental history search: typing narrows it,
// Ctrl+R steps to an older match, Enter puts the match on the input line,
// Esc or Ctrl+G gives the line back as it was (ok false).
func (lr *LineReader) reverseSearch(orig []rune) (line string, ok bool, err error) {
	var query []rune
	idx := len(lr.history) // index of the current match; len = none
	find := func(from int) {
		q := strings.ToLower(string(query))
		for i := from; i >= 0; i-- {
			if i < len(lr.history) && strings.Contains(strings.ToLower(lr.history[i]), q) {
				idx = i
				return
			}
		}
	}
	draw := func() {
		match := ""
		if idx < len(lr.history) {
			match = lr.history[idx]
		}
		consoleMu.Lock()
		savedPrompt, savedWidth := lr.prompt, lr.promptWidth
		lr.prompt = "\x1b[36m(搜索历史)\x1b[0m " + string(query) + " ❯ "
		lr.promptWidth = promptVisibleWidth(lr.prompt)
		m := []rune(match)
		lr.redrawLocked(m, len(m))
		lr.prompt, lr.promptWidth = savedPrompt, savedWidth
		consoleMu.Unlock()
	}
	draw()
	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			return "", false, err
		}
		switch {
		case r == 18: // Ctrl+R: older match
			if idx > 0 {
				find(idx - 1)
			}
		case r == 7: // Ctrl+G
			return string(orig), false, nil
		case r == 27:
			if lr.rawReader.Buffered() > 0 {
				// A key's sequence (an arrow): leave the search with the match.
				if next, _ := readInputRune(lr.rawReader); next == '[' {
					_, _, _ = readCSI(lr.rawReader)
				}
				return lr.searchResult(idx, orig)
			}
			return string(orig), false, nil
		case r == '\r' || r == '\n':
			return lr.searchResult(idx, orig)
		case r == 127 || r == 8:
			if len(query) > 0 {
				query = query[:len(query)-1]
				idx = len(lr.history)
				find(len(lr.history) - 1)
			}
		case r >= 32:
			query = append(query, r)
			from := idx
			if from >= len(lr.history) {
				from = len(lr.history) - 1
			}
			find(from)
		}
		draw()
	}
}

func (lr *LineReader) searchResult(idx int, orig []rune) (string, bool, error) {
	if idx < len(lr.history) {
		lr.histIdx = idx
		return lr.history[idx], true, nil
	}
	return string(orig), false, nil
}

// ---------------------------------------------------------------------------
// History file
// ---------------------------------------------------------------------------

// historyFileMax is how many entries the history file keeps.
const historyFileMax = 1000

var (
	historyMu   sync.Mutex
	historyPath string // "" = history is not persisted (tests, -p)
)

// SetHistoryFile makes the editor keep its history in path across restarts
// (one JSON string per line, so multi-line entries survive). Call it before
// New. The history used to live only in memory.
func SetHistoryFile(path string) {
	historyMu.Lock()
	historyPath = path
	historyMu.Unlock()
}

func loadHistory() []string {
	historyMu.Lock()
	path := historyPath
	historyMu.Unlock()
	return readHistoryFile(path)
}

func readHistoryFile(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var s string
		if json.Unmarshal(sc.Bytes(), &s) == nil && s != "" {
			out = append(out, s)
		}
	}
	if len(out) > historyFileMax {
		out = out[len(out)-historyFileMax:]
	}
	return out
}

// appendHistory adds line to the history file, compacting the file to the
// newest historyFileMax entries once it has grown to twice that.
func appendHistory(line string) {
	historyMu.Lock()
	defer historyMu.Unlock()
	if historyPath == "" {
		return
	}
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(historyPath), 0o700)
	f, err := os.OpenFile(historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	info, _ := f.Stat()
	_ = f.Close()
	if info != nil && info.Size() > 2*1024*1024 {
		entries := readHistoryFile(historyPath)
		var sb strings.Builder
		for _, e := range entries {
			if d, err := json.Marshal(e); err == nil {
				sb.Write(d)
				sb.WriteByte('\n')
			}
		}
		_ = os.WriteFile(historyPath, []byte(sb.String()), 0o600)
	}
}

// ---------------------------------------------------------------------------
// Multi-row input
// ---------------------------------------------------------------------------

// maxInputRows is the most rows the input is drawn on; a longer input falls
// back to the one-row window (a block taller than the screen cannot be
// redrawn in place).
const maxInputRows = 12

// inputRow is one drawn row of a multi-row input.
type inputRow struct {
	prefix string // the prompt on the first row, spaces under it after
	text   []rune
	start  int // buffer index of text[0]
}

// layoutInput splits buf into rows of at most w-1 cells after the prompt,
// breaking at newlines, and finds the cursor's row and its offset in it.
func layoutInput(prompt string, promptWidth int, buf []rune, cursor, w int) (rows []inputRow, curRow, curOff int) {
	avail := w - 1 - promptWidth
	if avail < 8 {
		avail = 8
	}
	indent := strings.Repeat(" ", promptWidth)
	row := inputRow{prefix: prompt}
	cells := 0
	for i, r := range buf {
		if i == cursor {
			curRow, curOff = len(rows), len(row.text)
		}
		if r == '\n' {
			rows = append(rows, row)
			row = inputRow{prefix: indent, start: i + 1}
			cells = 0
			continue
		}
		disp := r
		if r == '\t' {
			disp = ' '
		}
		cw := runeCellWidth(disp)
		if cells+cw > avail {
			rows = append(rows, row)
			row = inputRow{prefix: indent, start: i}
			cells = 0
			if i == cursor {
				curRow, curOff = len(rows), 0
			}
		}
		row.text = append(row.text, disp)
		cells += cw
	}
	if cursor >= len(buf) {
		curRow, curOff = len(rows), len(row.text)
	}
	rows = append(rows, row)
	return rows, curRow, curOff
}

// needsMultiRow reports whether buf is drawn on several rows: it has a
// newline or does not fit one row, and fits maxInputRows.
func (lr *LineReader) needsMultiRow(buf []rune, w int) bool {
	multi := false
	width := 0
	for _, r := range buf {
		if r == '\n' {
			multi = true
			break
		}
		width += runeCellWidth(r)
	}
	if !multi && width <= w-1-lr.promptWidth {
		return false
	}
	rows, _, _ := layoutInput(lr.prompt, lr.promptWidth, buf, len(buf), w)
	return len(rows) <= maxInputRows
}

// clearRowsLocked erases a multi-row input: up to its first row, then to
// the end of the screen.
func (lr *LineReader) clearRowsLocked() {
	if lr.cursorRow > 0 {
		termPrint(fmt.Sprintf("\x1b[%dA", lr.cursorRow))
	}
	termPrint("\x1b[0m\r\x1b[J")
	lr.drawnRows, lr.cursorRow = 0, 0
}

// redrawMultiLocked draws buf on several rows and puts the cursor on its row
// by re-printing that row up to it (the terminal's own width rules then
// place it, as in the one-row editor).
func (lr *LineReader) redrawMultiLocked(buf []rune, cursor, w int) {
	if lr.drawnRows > 0 {
		lr.clearRowsLocked()
	} else {
		termPrint("\x1b[0m\x1b[?25h\r\x1b[2K")
	}
	rows, curRow, curOff := layoutInput(lr.prompt, lr.promptWidth, buf, cursor, w)
	for i, row := range rows {
		if i > 0 {
			termPrint("\r\n")
		}
		termPrint(row.prefix + "\x1b[0m" + string(row.text))
	}
	if up := len(rows) - 1 - curRow; up > 0 {
		termPrint(fmt.Sprintf("\x1b[%dA", up))
	}
	termPrint("\r" + rows[curRow].prefix + "\x1b[0m" + string(rows[curRow].text[:curOff]))
	lr.drawnRows, lr.cursorRow = len(rows), curRow
	lr.lineDrawn = true
}
