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
	promptWidth    int // visible character width (excluding ANSI codes)
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
}

var consoleMu sync.Mutex
var activeReader *LineReader

// streamingActive is set while a background task owns the output area.
var streamingActive bool

// permInputCh, when non-nil, receives the next line typed by the user instead
// of being processed as a task. Used to route permission-prompt answers.
var permInputCh chan<- string

// SetPermInputCh registers a channel that will receive the very next input line.
// The channel must be buffered or have a receiver ready.
func SetPermInputCh(ch chan<- string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	permInputCh = ch
}

// ClearPermInputCh removes the current permission input channel.
func ClearPermInputCh() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	permInputCh = nil
}

// TakePermInputCh atomically returns and clears the current permission channel.
// Returns nil if none is set.
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
	return &LineReader{
		completer:   completer,
		prompt:      Prompt(),
		promptWidth: 2, // "❯ " visible chars
		placeholder: "(按 / 显示命令)",
	}
}

func PrintSafe(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	PrintAbove(s)
}

func normalizeOutputNewlines(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// printOutputLocked writes normal output. Must be called with consoleMu held.
func printOutputLocked(s string, ensureTrailingNewline bool) {
	fmt.Print(s)
	if ensureTrailingNewline && !strings.HasSuffix(s, "\r\n") {
		fmt.Print("\r\n")
	}
}

// PrintAbove prints output without leaving stale readline content behind.
func PrintAbove(s string) {
	s = normalizeOutputNewlines(s)
	consoleMu.Lock()
	defer consoleMu.Unlock()

	if activeReader == nil || !activeReader.reading {
		fmt.Print(s)
		return
	}

	activeReader.eraseLine()
	printOutputLocked(s, !strings.HasSuffix(s, "\r\n"))
	activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
}

// StreamPrint appends model/tool output in a way that does not steal the
// active readline cursor. Use this for streaming deltas instead of fmt.Print.
func StreamPrint(s string) {
	s = normalizeOutputNewlines(s)
	consoleMu.Lock()
	defer consoleMu.Unlock()
	printOutputLocked(s, false)
}

// PrintTransientStatus draws or clears a single status line in the output area.
// It is intended for spinners and other temporary indicators.
func PrintTransientStatus(s string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	if activeReader != nil && activeReader.reading {
		return
	}
	fmt.Print("\x1b[0m\x1b[?25h\r\x1b[K")
	fmt.Print(s)
}

// BeginOutput marks the start of a streaming task output section.
// The current prompt is cleared so streamed output can use normal scrollback.
func BeginOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	streamingActive = true
	if activeReader != nil && activeReader.reading {
		activeReader.eraseLine()
		fmt.Print("\n")
	} else {
		fmt.Print("\n")
	}
}

// EndOutput marks the end of a streaming task output section and redraws the box.
func EndOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	streamingActive = false
	printOutputLocked("\r\n", false)
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
}

func HasActiveInput() bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	return activeReader != nil && activeReader.reading
}

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
		term.Restore(int(os.Stdin.Fd()), oldState)
	}()

	// Always create a fresh buffered reader to avoid stale data from
	// previous ReadLine calls or from other stdin consumers (e.g. permission
	// prompts) that may have left the OS buffer in an inconsistent state.
	lr.rawReader = bufio.NewReader(os.Stdin)
	fmt.Print("\x1b[0m\x1b[?25h")

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

	var buf []rune
	cursor := 0
	lr.redraw(buf, cursor)

	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			return "", err
		}

		switch r {
		case 3:
			consoleMu.Lock()
			lr.eraseLine()
			consoleMu.Unlock()
			fmt.Print("\r\n")
			return "", ErrInterrupt
		case 4:
			if len(buf) == 0 {
				consoleMu.Lock()
				lr.eraseLine()
				consoleMu.Unlock()
				fmt.Print("\r\n")
				return "", ErrExit
			}
		case '\r', '\n':
			consoleMu.Lock()
			lr.eraseLine()
			consoleMu.Unlock()
			fmt.Print("\r\n")
			line := string(buf)
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
				lr.redraw(buf, cursor)
				line := string(buf)
				if strings.HasPrefix(line, "/") && lr.completer != nil {
					suggestions := lr.completer(line)
					if len(suggestions) > 0 && len(suggestions) <= 10 {
						lr.showInlineSuggestions(suggestions, lr.promptWidth+cursor)
					} else if len(suggestions) > 10 {
						lr.showCommandCountHint(len(suggestions), lr.promptWidth+cursor)
					}
				}
			}
		case 27:
			lr.resetCompletionCycle()
			if err := lr.handleEscape(&buf, &cursor); err != nil {
				return "", err
			}
		case '\t':
			lr.complete(&buf, &cursor)
		default:
			if r >= 32 {
				lr.resetCompletionCycle()
				buf = append(buf, 0)
				copy(buf[cursor+1:], buf[cursor:])
				buf[cursor] = r
				cursor++
				lr.redraw(buf, cursor)
				line := string(buf)
				if strings.HasPrefix(line, "/") && lr.completer != nil {
					suggestions := lr.completer(line)
					if len(suggestions) > 0 && len(suggestions) <= 10 {
						lr.showInlineSuggestions(suggestions, lr.promptWidth+cursor)
					} else if len(suggestions) > 10 {
						lr.showCommandCountHint(len(suggestions), lr.promptWidth+cursor)
					}
				}
			}
		}
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
	// CJK/full-width ranges used by terminals as 2-cell characters.
	if (r >= 0x1100 && r <= 0x115F) ||
		(r >= 0x2329 && r <= 0x232A) ||
		(r >= 0x2E80 && r <= 0xA4CF) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE10 && r <= 0xFE19) ||
		(r >= 0xFE30 && r <= 0xFE6F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x1F300 && r <= 0x1FAFF) {
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

func inputDisplayWindow(buf []rune, cursor, maxCols int) ([]rune, int, int) {
	if maxCols < 1 {
		maxCols = 1
	}
	start := 0
	for start < cursor && runesCellWidth(buf[start:cursor]) > maxCols {
		start++
	}
	end := start
	used := 0
	for end < len(buf) {
		cw := runeCellWidth(buf[end])
		if used+cw > maxCols {
			break
		}
		used += cw
		end++
	}
	disp := buf[start:end]
	cursorCells := runesCellWidth(buf[start:cursor])
	return disp, cursorCells, used
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

func (lr *LineReader) handleEscape(buf *[]rune, cursor *int) error {
	first, err := readInputRune(lr.rawReader)
	if err != nil {
		return err
	}
	if first != '[' {
		// Not an ANSI escape sequence (e.g. Alt+key on some terminals
		// sends ESC followed by the modified key). Unread the byte so
		// it can be processed as a regular character in the main loop.
		lr.rawReader.UnreadRune()
		return nil
	}
	second, err := readInputRune(lr.rawReader)
	if err != nil {
		return err
	}

	switch second {
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
	case '3':
		third, err := readInputRune(lr.rawReader)
		if err != nil {
			return err
		}
		if third == '~' && *cursor < len(*buf) {
			copy((*buf)[*cursor:], (*buf)[*cursor+1:])
			*buf = (*buf)[:len(*buf)-1]
			lr.redraw(*buf, *cursor)
		}
	}
	return nil
}

func (lr *LineReader) redraw(buf []rune, cursor int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.redrawLocked(buf, cursor)
}

// eraseLine clears the current readline prompt. Must be called with consoleMu held.
func (lr *LineReader) eraseLine() {
	fmt.Print("\x1b[0m\x1b[?25h\r\x1b[2K")
	lr.lineDrawn = false
}

func (lr *LineReader) redrawLocked(buf []rune, cursor int) {
	lr.renderBuf = append(lr.renderBuf[:0], buf...)
	lr.renderCursor = cursor
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 20 {
		w = 80
	}

	lr.eraseLine()

	maxInputVis := w - lr.promptWidth - 1
	if maxInputVis < 1 {
		maxInputVis = 1
	}
	dispBuf, cursorCells, _ := inputDisplayWindow(buf, cursor, maxInputVis)
	fmt.Print("\x1b[0m\x1b[?25h")
	fmt.Print(lr.prompt)
	if len(buf) == 0 && lr.placeholder != "" {
		ph := []rune(lr.placeholder)
		ph, _ = truncateRunesByCells(ph, maxInputVis)
		fmt.Print("\x1b[90m" + string(ph) + "\x1b[0m")
	} else {
		fmt.Print("\x1b[0m" + string(dispBuf) + "\x1b[0m")
	}
	fmt.Print("\r")
	if lr.promptWidth+cursorCells > 0 {
		fmt.Printf("\x1b[%dC", lr.promptWidth+cursorCells)
	}
	lr.lineDrawn = true
}

func (lr *LineReader) showInlineSuggestions(suggestions []string, cursorOffset int) {
	const maxInline = 8
	consoleMu.Lock()
	defer consoleMu.Unlock()

	printHints := func() {
		fmt.Print("\x1b[90m  ")
		shown := 0
		for _, s := range suggestions {
			if shown >= maxInline {
				break
			}
			if idx := strings.IndexByte(s, '\t'); idx >= 0 {
				fmt.Print(s[:idx] + "  ")
			} else {
				fmt.Print(s + "  ")
			}
			shown++
		}
		if len(suggestions) > maxInline {
			fmt.Printf("...(+%d)", len(suggestions)-maxInline)
		}
		fmt.Print("\x1b[0m")
	}

	lr.eraseLine()
	printHints()
	fmt.Print("\r\n")
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) showCommandCountHint(count int, cursorOffset int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	printHint := func() {
		fmt.Printf("\x1b[90m  (按 Tab 查看 %d 个命令)\x1b[0m", count)
	}

	lr.eraseLine()
	printHint()
	fmt.Print("\r\n")
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) historyUp(buf *[]rune, cursor *int) {
	if len(lr.history) == 0 {
		return
	}
	if lr.histIdx > 0 {
		lr.histIdx--
		*buf = []rune(lr.history[lr.histIdx])
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
	}
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
	// Extract command text (before \t if annotated)
	texts := completionTexts(suggestions)
	if len(suggestions) == 1 {
		*buf = []rune(texts[0])
		*cursor = len(*buf)
		lr.resetCompletionCycle()
		lr.redraw(*buf, *cursor)
		return
	}
	common := commonPrefix(texts)
	if len(common) > len(line) {
		*buf = []rune(common)
		*cursor = len(*buf)
		lr.completionBase = common
		lr.completionList = append(lr.completionList[:0], texts...)
		lr.completionIdx = -1
		lr.redraw(*buf, *cursor)
		return
	}
	lr.completionBase = line
	lr.completionList = append(lr.completionList[:0], texts...)
	lr.completionIdx = -1
	// First Tab shows a compact hint; repeated Tab cycles through candidates.
	lr.showInlineSuggestions(suggestions, lr.promptWidth+*cursor)
}

func (lr *LineReader) advanceCompletionCycle(buf *[]rune, cursor *int) bool {
	line := string(*buf)
	next, idx, ok := completionCycleNext(line, lr.completionBase, lr.completionList, lr.completionIdx)
	if !ok {
		lr.resetCompletionCycle()
		return false
	}
	lr.completionIdx = idx
	*buf = []rune(next)
	*cursor = len(*buf)
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
	lr.completionBase = ""
	lr.completionList = nil
	lr.completionIdx = -1
}

func completionTexts(suggestions []string) []string {
	texts := make([]string, len(suggestions))
	for i, s := range suggestions {
		if idx := strings.IndexByte(s, '\t'); idx >= 0 {
			texts[i] = s[:idx]
		} else {
			texts[i] = s
		}
	}
	return texts
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
