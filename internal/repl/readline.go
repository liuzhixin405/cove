package repl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/term"
)

type Completer func(input string) []string

type LineReader struct {
	history        []string
	histIdx        int
	completer      Completer
	prompt         string
	promptWidth    int
	placeholder    string
	fallbackReader *bufio.Reader
	rawReader      *bufio.Reader
	reading        bool
	renderBuf      []rune
	renderCursor   int
	lineDrawn      bool
	completionBase string
	completionList []string
	completionIdx  int
	activeHint     string
}

var consoleMu sync.Mutex
var activeReader *LineReader
var streamingActive bool

// streamMidLine records that streamed output left the cursor after text on
// its row. Without it, anything that starts a new block during a stream — a
// notice, the permission box's input-line redraw (\r ESC[2K) — landed on that
// row: a notice was glued to the model's unfinished sentence, and the redraw
// erased the partial line outright.
var streamMidLine bool
var permInputCh chan<- string

func SetPermInputCh(ch chan<- string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	permInputCh = ch
}

func ClearPermInputCh() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	permInputCh = nil
}

func TakePermInputCh() chan<- string {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	ch := permInputCh
	permInputCh = nil
	return ch
}

var ErrExit = fmt.Errorf("exit")
var ErrInterrupt = fmt.Errorf("interrupt")

func New(completer Completer) *LineReader {
	lr := &LineReader{
		completer:   completer,
		placeholder: "(按 / 显示命令)",
	}
	lr.SetPrompt(Prompt()) // derives promptWidth correctly (skips ANSI codes)
	return lr
}

// SetPrompt changes the prompt string and recalculates its visual width.
// ANSI escape sequences are skipped so the width reflects only visible cells.
func (lr *LineReader) SetPrompt(p string) {
	lr.prompt = p
	w := 0
	inAnsi := false
	for _, r := range p {
		if r == '\x1b' {
			inAnsi = true
			continue
		}
		if inAnsi {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
			continue
		}
		w += runeCellWidth(r)
	}
	lr.promptWidth = w
}

func PrintSafe(format string, args ...any) {
	PrintAbove(fmt.Sprintf(format, args...))
}

func normalizeOutputNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func printOutputLocked(s string, ensureTrailingNewline bool) {
	fmt.Print(s)
	if ensureTrailingNewline && !strings.HasSuffix(s, "\r\n") {
		fmt.Print("\r\n")
	}
}

func PrintAbove(s string) {
	s = normalizeOutputNewlines(s)
	consoleMu.Lock()
	defer consoleMu.Unlock()

	// While a task is streaming we never keep an editable input line on screen,
	// so print inline without erasing/redrawing (which would corrupt partial
	// streamed lines that don't end in a newline).
	if streamingActive {
		breakStreamLineLocked()
		printOutputLocked(s, !strings.HasSuffix(s, "\r\n"))
		streamMidLine = false
		return
	}

	if activeReader == nil || !activeReader.reading {
		fmt.Print(s)
		return
	}

	activeReader.eraseLineLocked()
	printOutputLocked(s, !strings.HasSuffix(s, "\r\n"))
	activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
}

func StreamPrint(s string) {
	s = normalizeOutputNewlines(s)

	consoleMu.Lock()
	defer consoleMu.Unlock()
	// During streaming, print the chunk verbatim. Erasing/redrawing the input
	// line here is what corrupted the "thinking"/answer stream, because a chunk
	// without a trailing newline shares the current terminal line and the next
	// erase (\r\x1b[2K) wiped it.
	if streamingActive {
		fmt.Print(s)
		streamMidLine = endsMidLine(streamMidLine, s)
		return
	}
	if activeReader != nil && activeReader.reading {
		activeReader.eraseLineLocked()
		fmt.Print(s)
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
		return
	}
	fmt.Print(s)
}

func PrintTransientStatus(s string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	// The spinner runs during streaming; just overwrite the current line in
	// place without touching any input-line state.
	if streamingActive {
		fmt.Print("\x1b[0m\x1b[?25h\r\x1b[K" + s)
		streamMidLine = s != ""
		return
	}

	if activeReader != nil && activeReader.reading {
		activeReader.eraseLineLocked()
		fmt.Print("\x1b[0m\x1b[?25h" + s)
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
		return
	}
	fmt.Print("\x1b[0m\x1b[?25h\r\x1b[K" + s)
}

func BeginOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	// Erase any idle input line BEFORE marking streaming active (eraseLineLocked
	// is a no-op once streamingActive is set).
	if activeReader != nil && activeReader.reading {
		activeReader.eraseLineLocked()
	}
	streamingActive = true
	streamMidLine = false
	fmt.Print("\n")
}

// BeginPromptInput temporarily suspends streaming-output suppression so an
// interactive prompt (e.g. a permission y/n/a question) can draw and echo the
// input line normally. The engine is blocked awaiting the answer, so no
// streaming output is produced meanwhile. Pair with EndPromptInput.
func BeginPromptInput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	breakStreamLineLocked()
	streamingActive = false
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
}

// EndPromptInput restores streaming-output suppression after an interactive
// prompt has been answered.
func EndPromptInput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if activeReader != nil && activeReader.reading {
		activeReader.eraseLineLocked()
	}
	streamingActive = true
}

func EndOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	streamingActive = false
	streamMidLine = false
	printOutputLocked("\r\n", false)
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
}

// breakStreamLineLocked moves to a fresh row if the stream left text on the
// current one. Callers hold consoleMu.
func breakStreamLineLocked() {
	if streamingActive && streamMidLine {
		fmt.Print("\r\n")
		streamMidLine = false
	}
}

// endsMidLine reports whether the cursor is after text on its row once s has
// been printed, given whether it was before. Escape sequences are not text,
// and a bare \r leaves the row's text in place.
func endsMidLine(was bool, s string) bool {
	mid := was
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == 0x1b:
			// Skip a CSI sequence, the only kind that reaches this point:
			// untrusted text is sanitised before it is printed.
			if i+1 < len(s) && s[i+1] == '[' {
				i += 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
			}
		case c == '\n':
			mid = false
		case c == '\r':
		default:
			mid = true
		}
	}
	return mid
}

func HasActiveInput() bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	return activeReader != nil && activeReader.reading
}

// waitStreamingDone is obsolete: streaming output no longer erases the input
// line, so ReadLine may enter raw mode immediately even while a task streams
// (this is what enables blind type-ahead into the task queue). Kept removed to
// avoid the multi-second cooked-mode stall it used to impose on every
// mid-task ReadLine.

func (lr *LineReader) ReadLine() (string, error) {
	if shouldUseFallbackReadline() {
		return lr.fallbackRead()
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return lr.fallbackRead()
	}
	defer func() {
		fmt.Print("\x1b[0m\x1b[?25h")
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}()

	// The reader lives as long as the LineReader. It used to be rebuilt on
	// every call, so whatever the previous read had already buffered — the
	// rest of a paste, keys typed ahead — was thrown away with it.
	if lr.rawReader == nil {
		lr.rawReader = bufio.NewReaderSize(os.Stdin, rawInputBufferSize)
	}
	// Bracketed paste makes the terminal wrap pasted text in ESC[200~ …
	// ESC[201~, so its newlines can be told apart from Enter. Terminals that
	// do not support it ignore the request.
	fmt.Print("\x1b[0m\x1b[?25h\x1b[?2004h")
	defer fmt.Print("\x1b[?2004l")

	consoleMu.Lock()
	activeReader = lr
	lr.reading = true
	lr.renderBuf = nil
	lr.renderCursor = 0
	lr.lineDrawn = false
	consoleMu.Unlock()

	defer func() {
		consoleMu.Lock()
		if activeReader == lr {
			activeReader = nil
		}
		lr.reading = false
		lr.lineDrawn = false
		consoleMu.Unlock()
	}()

	return lr.editLine()
}

// editLine runs the raw-mode editor on lr.rawReader until a line is submitted.
func (lr *LineReader) editLine() (string, error) {
	var buf []rune
	cursor := 0
	lr.redraw(buf, cursor)

	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			return lr.endOfInput(buf, err)
		}

		switch r {
		case 3:
			consoleMu.Lock()
			lr.eraseLineLocked()
			consoleMu.Unlock()
			fmt.Print("\r\n")
			return "", ErrInterrupt
		case 4:
			if len(buf) == 0 {
				consoleMu.Lock()
				lr.eraseLineLocked()
				consoleMu.Unlock()
				fmt.Print("\r\n")
				return "", ErrExit
			}
		case '\r', '\n':
			// A person cannot press Enter and further keys within one read,
			// so a newline with input already waiting behind it is part of a
			// paste on a console without bracketed paste (conhost). It used
			// to submit each pasted line as a separate message.
			if lr.rawReader.Buffered() > 0 {
				if r == '\r' {
					lr.skipRune('\n')
				}
				buf, cursor = insertRunes(buf, cursor, []rune{'\n'})
				lr.refresh(buf, cursor)
				continue
			}
			line := string(buf)
			consoleMu.Lock()
			lr.eraseLineLocked()
			// 关键点：在按下回车后，先把用户输入的内容打印到终端，使之成为历史可见内容。
			// 但在流式输出进行中（盲打补充输入）时不要回显，否则会把提示符+内容插进流式文本里造成错乱。
			if !streamingActive {
				fmt.Print(lr.prompt + normalizeOutputNewlines(line) + "\r\n")
			}
			consoleMu.Unlock()

			if line != "" && (len(lr.history) == 0 || lr.history[len(lr.history)-1] != line) {
				lr.history = append(lr.history, line)
			}
			lr.histIdx = len(lr.history)
			return line, nil
		case 127, 8:
			lr.resetCompletionCycle()
			if cursor > 0 {
				copy(buf[cursor-1:], buf[cursor:])
				buf = buf[:len(buf)-1]
				cursor--
				lr.refresh(buf, cursor)
			}
		case 27:
			lr.resetCompletionCycle()
			if err := lr.handleEscape(&buf, &cursor); err != nil {
				return lr.endOfInput(buf, err)
			}
		case '\t':
			if lr.rawReader.Buffered() > 0 {
				// A tab inside pasted text is text, not a completion request.
				buf, cursor = insertRunes(buf, cursor, []rune{'\t'})
				lr.refresh(buf, cursor)
				continue
			}
			lr.complete(&buf, &cursor)
		default:
			if r >= 32 {
				lr.resetCompletionCycle()
				buf, cursor = insertRunes(buf, cursor, []rune{r})
				lr.refresh(buf, cursor)
			}
		}
	}
}

// rawInputBufferSize is large so that a pasted block normally arrives in one
// buffer fill: the paste heuristic in editLine looks at what is buffered.
const rawInputBufferSize = 64 * 1024

// endOfInput maps a read error to what the REPL loop expects. A closed stdin
// (terminal gone, pipe ended) used to come back as io.EOF, which the loop
// treats as "reinitialise and read again" — forever, at full CPU, printing the
// same error line. Text typed before the end is still delivered; the next call
// then reports ErrExit.
func (lr *LineReader) endOfInput(buf []rune, err error) (string, error) {
	if err != io.EOF {
		return "", err
	}
	consoleMu.Lock()
	lr.eraseLineLocked()
	consoleMu.Unlock()
	fmt.Print("\r\n")
	if len(buf) > 0 {
		return string(buf), nil
	}
	return "", ErrExit
}

// refresh redraws the input line and its command hints, unless more input is
// already waiting. Redrawing after every rune of a paste made a long paste
// quadratic (each redraw copies and measures the whole buffer); the line is
// drawn once the burst has been consumed.
func (lr *LineReader) refresh(buf []rune, cursor int) {
	if lr.rawReader != nil && lr.rawReader.Buffered() > 0 {
		consoleMu.Lock()
		lr.renderBuf = append(lr.renderBuf[:0], buf...)
		lr.renderCursor = cursor
		consoleMu.Unlock()
		return
	}
	lr.redraw(buf, cursor)
	if lr.completer == nil || len(buf) == 0 || buf[0] != '/' {
		return
	}
	line := string(buf)
	suggestions := lr.completer(line)
	if len(suggestions) > 0 && len(suggestions) <= 10 {
		lr.showInlineSuggestions(suggestions, lr.promptWidth+cursor)
	} else if len(suggestions) > 10 {
		lr.showCommandCountHint(len(suggestions), lr.promptWidth+cursor)
	}
}

// insertRunes inserts rs at cursor and returns the new buffer and cursor.
func insertRunes(buf []rune, cursor int, rs []rune) ([]rune, int) {
	buf = append(buf, rs...)
	copy(buf[cursor+len(rs):], buf[cursor:len(buf)-len(rs)])
	copy(buf[cursor:], rs)
	return buf, cursor + len(rs)
}

// skipRune consumes the next rune if it is want and already buffered.
func (lr *LineReader) skipRune(want rune) {
	if lr.rawReader.Buffered() == 0 {
		return
	}
	if r, _, err := lr.rawReader.ReadRune(); err == nil && r != want {
		_ = lr.rawReader.UnreadRune()
	}
}

func readInputRune(r *bufio.Reader) (rune, error) {
	ch, _, err := r.ReadRune()
	return ch, err
}

func runeCellWidth(r rune) int {
	if r == 0 || r == '\n' || r == '\r' || r == '\t' {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if (r >= 0x1100 && r <= 0x115F) ||
		(r >= 0x2329 && r <= 0x232A) ||
		(r >= 0x2E80 && r <= 0xA4CF) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE10 && r <= 0xFE19) ||
		(r >= 0xFE30 && r <= 0xFE6F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x1F300 && r <= 0x1FAFF) ||
		// CJK Extension B and later (planes 2 and 3). Measured as one column
		// they overflowed the visible window, the terminal soft-wrapped the
		// input line, and the next redraw left a ghost row.
		(r >= 0x20000 && r <= 0x3FFFD) {
		return 2
	}
	return 1
}

func runesCellWidth(rs []rune) int {
	w := 0
	for _, r := range rs {
		w += runeCellWidth(r)
	}
	return w
}

func inputDisplayWindow(buf []rune, cursor, maxCols int) (disp []rune, cursorCells, used, start int) {
	if maxCols < 1 {
		maxCols = 1
	}
	// Walk back from the cursor to the widest prefix that fits. It used to
	// advance start one rune at a time and re-measure buf[start:cursor] at
	// each step, which is quadratic in the input length on every keystroke:
	// a pasted log froze the editor.
	start = cursor
	cursorCells = 0
	for start > 0 {
		cw := runeCellWidth(buf[start-1])
		if cursorCells+cw > maxCols {
			break
		}
		cursorCells += cw
		start--
	}
	end := start
	used = 0
	for end < len(buf) {
		cw := runeCellWidth(buf[end])
		if used+cw > maxCols {
			break
		}
		used += cw
		end++
	}
	disp = buf[start:end]
	return disp, cursorCells, used, start
}

func truncateAnsi(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	var sb strings.Builder
	inAnsi := false
	vis := 0
	for _, r := range s {
		if r == '\x1b' {
			inAnsi = true
			sb.WriteRune(r)
			continue
		}
		if inAnsi {
			sb.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
			continue
		}
		cw := runeCellWidth(r)
		if vis+cw > maxCols {
			break
		}
		vis += cw
		sb.WriteRune(r)
	}
	// ensure ansi resets if truncated
	sb.WriteString("\x1b[0m")
	return sb.String()
}

func truncateRunesByCells(rs []rune, maxCols int) ([]rune, int) {
	used := 0
	end := 0
	for end < len(rs) {
		cw := runeCellWidth(rs[end])
		if used+cw > maxCols {
			break
		}
		used += cw
		end++
	}
	return rs[:end], used
}

// handleEscape consumes one escape sequence and applies the key it encodes.
//
// It used to understand only a bare ESC [ <letter>. Any sequence with
// parameters — Ctrl/Shift/Alt + arrow ("ESC[1;5D", which Windows Terminal
// sends for Ctrl+Left), Home/End as "ESC[1~"/"ESC[4~", Insert, F5 — stopped
// after the first parameter byte and the rest (";5D", "~") was typed into the
// input as text. SS3 keys (ESC O H for Home in application cursor mode, F1–F4)
// were typed in whole. Now the full sequence is read and an unknown one is
// dropped.
func (lr *LineReader) handleEscape(buf *[]rune, cursor *int) error {
	first, err := readInputRune(lr.rawReader)
	if err != nil {
		return err
	}
	switch first {
	case '[':
		params, final, err := readCSI(lr.rawReader)
		if err != nil {
			return err
		}
		if final == '~' && csiKey(params) == 200 {
			return lr.readBracketedPaste(buf, cursor)
		}
		lr.applyKey(buf, cursor, params, final)
	case 'O':
		final, err := readInputRune(lr.rawReader)
		if err != nil {
			return err
		}
		lr.applyKey(buf, cursor, "", final)
	default:
		_ = lr.rawReader.UnreadRune()
	}
	return nil
}

// readCSI reads the rest of a control sequence after "ESC [": parameter
// bytes, intermediate bytes and the final byte. A byte that cannot belong to a
// sequence ends it early and is left unread; final is then 0.
func readCSI(r *bufio.Reader) (params string, final rune, err error) {
	var sb strings.Builder
	for {
		c, err := readInputRune(r)
		if err != nil {
			return sb.String(), 0, err
		}
		switch {
		case c >= 0x20 && c <= 0x3f:
			sb.WriteRune(c)
		case c >= 0x40 && c <= 0x7e:
			return sb.String(), c, nil
		default:
			_ = r.UnreadRune()
			return sb.String(), 0, nil
		}
	}
}

// csiKey returns the first numeric parameter of a sequence ("1;5" -> 1), or
// -1 when there is none.
func csiKey(params string) int {
	head, _, _ := strings.Cut(params, ";")
	if head == "" {
		return -1
	}
	n := 0
	for _, c := range head {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// applyKey performs the editing action for a decoded key. Modifier parameters
// (Ctrl/Shift/Alt) are accepted and ignored: Ctrl+Left moves like Left rather
// than typing ";5D". Keys the editor has no use for do nothing.
func (lr *LineReader) applyKey(buf *[]rune, cursor *int, params string, final rune) {
	switch final {
	case 'A':
		lr.historyUp(buf, cursor)
	case 'B':
		lr.historyDown(buf, cursor)
	case 'C':
		if *cursor < len(*buf) {
			*cursor = *cursor + 1
			lr.redraw(*buf, *cursor)
		}
	case 'D':
		if *cursor > 0 {
			*cursor = *cursor - 1
			lr.redraw(*buf, *cursor)
		}
	case 'H':
		*cursor = 0
		lr.redraw(*buf, *cursor)
	case 'F':
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
	case '~':
		switch csiKey(params) {
		case 1, 7:
			*cursor = 0
			lr.redraw(*buf, *cursor)
		case 4, 8:
			*cursor = len(*buf)
			lr.redraw(*buf, *cursor)
		case 3:
			if *cursor < len(*buf) {
				copy((*buf)[*cursor:], (*buf)[*cursor+1:])
				*buf = (*buf)[:len(*buf)-1]
				lr.redraw(*buf, *cursor)
			}
		}
	}
}

// readBracketedPaste inserts everything up to the closing ESC[201~ as text.
// Newlines in it become part of the message instead of submitting it, and the
// line is drawn once at the end rather than once per pasted rune.
func (lr *LineReader) readBracketedPaste(buf *[]rune, cursor *int) error {
	var pasted []rune
	defer func() {
		*buf, *cursor = insertRunes(*buf, *cursor, pasted)
		lr.redraw(*buf, *cursor)
	}()
	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			return err
		}
		switch {
		case r == 27:
			next, err := readInputRune(lr.rawReader)
			if err != nil {
				return err
			}
			if next != '[' {
				_ = lr.rawReader.UnreadRune()
				continue
			}
			params, final, err := readCSI(lr.rawReader)
			if err != nil {
				return err
			}
			if final == '~' && csiKey(params) == 201 {
				return nil
			}
		case r == '\r':
			lr.skipRune('\n')
			pasted = append(pasted, '\n')
		case r == '\n' || r == '\t':
			pasted = append(pasted, r)
		case r >= 32 && r != 127:
			pasted = append(pasted, r)
		}
	}
}

func (lr *LineReader) redraw(buf []rune, cursor int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.activeHint = ""
	lr.redrawLocked(buf, cursor)
}

func (lr *LineReader) eraseLineLocked() {
	// While streaming, the input line is never drawn, so the current terminal
	// line holds streamed output; erasing it would corrupt the stream.
	if streamingActive {
		lr.lineDrawn = false
		return
	}
	fmt.Print("\x1b[0m\x1b[?25h\r\x1b[2K")
	lr.lineDrawn = false
}

func (lr *LineReader) redrawLocked(buf []rune, cursor int) {
	lr.renderBuf = append(lr.renderBuf[:0], buf...)
	lr.renderCursor = cursor
	if streamingActive {
		return
	}
	// Draw on the current line.
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 20 {
		w = 80
	}
	fmt.Print("\x1b[0m\x1b[?25h\r\x1b[2K")
	maxVis := w - lr.promptWidth - 1
	if maxVis < 1 {
		maxVis = 1
	}
	shown := displayRunes(buf)
	disp, cells, _, start := inputDisplayWindow(shown, cursor, maxVis)
	fmt.Print(lr.prompt)
	if len(buf) == 0 && lr.placeholder != "" {
		ph, _ := truncateRunesByCells([]rune(lr.placeholder), maxVis)
		fmt.Print("\x1b[90m" + string(ph) + "\x1b[0m")
	} else {
		fmt.Print("\x1b[0m" + string(disp) + "\x1b[0m")
		if lr.activeHint != "" {
			rem := w - lr.promptWidth - cells - 1
			fmt.Print(truncateAnsi(lr.activeHint, rem))
		}
	}
	// Position the cursor by re-emitting the prompt plus the visible text to the
	// left of the cursor, letting the terminal advance the cursor with its own
	// width rules. This avoids the half-cell drift that plain column arithmetic
	// (\x1b[NC) causes with East Asian ambiguous-width glyphs such as the prompt
	// arrow when running in a CJK terminal.
	fmt.Print("\r")
	left := shown[start:cursor]
	fmt.Print(lr.prompt + "\x1b[0m" + string(left))
	lr.lineDrawn = true
}

// displayRunes maps the runes of a pasted block that cannot be drawn inside a
// one-row editor: a newline would move the cursor off the row (and, being a
// bare \n in raw mode, staircase the screen), and a tab jumps to a tab stop
// the width arithmetic knows nothing about. The mapping is one rune for one,
// so cursor indexes stay valid. The buffer itself is untouched.
func displayRunes(buf []rune) []rune {
	var out []rune
	for i, r := range buf {
		if r != '\n' && r != '\t' {
			continue
		}
		if out == nil {
			out = append([]rune(nil), buf...)
		}
		if r == '\n' {
			out[i] = '↵'
		} else {
			out[i] = ' '
		}
	}
	if out == nil {
		return buf
	}
	return out
}

func (lr *LineReader) showInlineSuggestions(suggestions []string, offset int) {
	const max = 8
	consoleMu.Lock()
	defer consoleMu.Unlock()
	printHints := func() string {
		var sb strings.Builder
		sb.WriteString("\x1b[90m  ")
		for i, s := range suggestions {
			if i >= max {
				break
			}
			text := s
			if idx := strings.IndexByte(s, '\t'); idx >= 0 {
				text = s[:idx]
			}
			sb.WriteString(text + "  ")
		}
		if len(suggestions) > max {
			fmt.Fprintf(&sb, "...(+%d)", len(suggestions)-max)
		}
		sb.WriteString("\x1b[0m")
		return sb.String()
	}
	lr.activeHint = printHints()
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) showCommandCountHint(count int, offset int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	hint := fmt.Sprintf("\x1b[90m  (按 Tab 查看 %d 个命令)\x1b[0m", count)
	lr.activeHint = hint
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) historyUp(buf *[]rune, cursor *int) {
	if len(lr.history) == 0 || lr.histIdx <= 0 {
		return
	}
	lr.histIdx--
	*buf = []rune(lr.history[lr.histIdx])
	*cursor = len(*buf)
	lr.redraw(*buf, *cursor)
}

func (lr *LineReader) historyDown(buf *[]rune, cursor *int) {
	if lr.histIdx < len(lr.history)-1 {
		lr.histIdx++
		*buf = []rune(lr.history[lr.histIdx])
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
	} else if lr.histIdx == len(lr.history)-1 {
		lr.histIdx = len(lr.history)
		*buf = nil
		*cursor = 0
		lr.redraw(*buf, *cursor)
	}
}

func (lr *LineReader) complete(buf *[]rune, cursor *int) {
	if lr.advanceCompletionCycle(buf, cursor) {
		return
	}
	if lr.completer == nil {
		return
	}
	line := string(*buf)
	suggestions := lr.completer(line)
	if len(suggestions) == 0 {
		lr.resetCompletionCycle()
		return
	}
	texts := completionTexts(suggestions)
	if len(suggestions) == 1 {
		*buf, *cursor = []rune(texts[0]), len(texts[0])
		lr.resetCompletionCycle()
		lr.redraw(*buf, *cursor)
		return
	}
	common := commonPrefix(texts)
	if len(common) > len(line) {
		*buf, *cursor = []rune(common), len(common)
		lr.completionBase, lr.completionList, lr.completionIdx = common, texts, -1
		lr.redraw(*buf, *cursor)
		return
	}
	lr.completionBase, lr.completionList, lr.completionIdx = line, texts, -1
	lr.showInlineSuggestions(suggestions, lr.promptWidth+*cursor)
}

func (lr *LineReader) advanceCompletionCycle(buf *[]rune, cursor *int) bool {
	next, idx, ok := completionCycleNext(string(*buf), lr.completionBase, lr.completionList, lr.completionIdx)
	if !ok {
		lr.resetCompletionCycle()
		return false
	}
	lr.completionIdx = idx
	*buf, *cursor = []rune(next), len(next)
	lr.redraw(*buf, *cursor)
	return true
}

func completionCycleNext(line, base string, list []string, idx int) (string, int, bool) {
	if len(list) == 0 {
		return "", idx, false
	}
	if line != base {
		current := ""
		if idx >= 0 && idx < len(list) {
			current = list[idx]
		}
		if line != current {
			return "", idx, false
		}
	}
	nextIdx := (idx + 1) % len(list)
	return list[nextIdx], nextIdx, true
}

func (lr *LineReader) resetCompletionCycle() {
	lr.completionBase, lr.completionList, lr.completionIdx = "", nil, -1
}

func completionTexts(ss []string) []string {
	res := make([]string, len(ss))
	for i, s := range ss {
		text := s
		if idx := strings.IndexByte(s, '\t'); idx >= 0 {
			text = s[:idx]
		}
		res[i] = text
	}
	return res
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

func (lr *LineReader) fallbackRead() (string, error) {
	if lr.fallbackReader == nil {
		lr.fallbackReader = bufio.NewReader(os.Stdin)
	}
	fmt.Print(lr.prompt)
	line, err := lr.fallbackReader.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				return trimmed, nil
			}
			fmt.Print("\n")
			return "", ErrExit
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ---------------------------------------------------------------------------
// The termui.Console protocol
// ---------------------------------------------------------------------------

// This package and internal/termui both grew the same console protocol —
// erase the input line, print, redraw — but only this one knows whether there
// is an input line on screen. So the ~140 call sites that go through termui
// printed on top of the prompt while the editor's own writes did the right
// thing.
//
// These methods let termui delegate to the editor instead. They are the same
// package-level functions above, exposed as a value termui can hold without
// importing this package (it declares the interface, we satisfy it).

// PrintAbove implements termui.Console.
func (lr *LineReader) PrintAbove(s string) { PrintAbove(s) }

// StreamPrint implements termui.Console.
func (lr *LineReader) StreamPrint(s string) { StreamPrint(s) }

// Transient implements termui.Console.
func (lr *LineReader) Transient(s string) { PrintTransientStatus(s) }

// BeginOutput implements termui.Console.
func (lr *LineReader) BeginOutput() { BeginOutput() }

// EndOutput implements termui.Console.
func (lr *LineReader) EndOutput() { EndOutput() }
