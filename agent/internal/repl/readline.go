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
	boxDrawn       bool // true while the 3-line input box is visible on screen
	termW          int  // terminal width captured when scroll region was set up
	termH          int  // terminal height when scroll region active (0 = no region)
}

var consoleMu sync.Mutex
var activeReader *LineReader

// streamingActive is set while a background task owns the output area.
// When true, readline redraws are suppressed so task output is not clobbered.
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

// PrintAbove prints output above the fixed input box without disturbing it.
// When the scroll region is active, it saves the cursor (in the box), moves to
// the scroll-region bottom, prints the content so it scrolls up within the
// region, then restores the cursor back to the box.
func PrintAbove(s string) {
	s = strings.ReplaceAll(s, "\n", "\r\n")
	consoleMu.Lock()
	defer consoleMu.Unlock()

	if streamingActive {
		// Output is already flowing in the scroll region; just append.
		fmt.Print(s)
		return
	}
	if activeReader == nil || !activeReader.reading {
		fmt.Print(s)
		return
	}

	if activeReader.termH > 0 {
		// Scroll region active: save cursor (box position), go to scroll
		// region bottom, print, then restore cursor back to box.
		fmt.Print("\x1b7")
		fmt.Printf("\x1b[%d;1H", activeReader.termH-3)
		fmt.Print(s)
		if !strings.HasSuffix(s, "\r\n") {
			fmt.Print("\r\n")
		}
		fmt.Print("\x1b8")
		return
	}

	// Fallback: erase box, print, redraw box.
	activeReader.eraseBox()
	fmt.Print(s)
	if !strings.HasSuffix(s, "\r\n") {
		fmt.Print("\r\n")
	}
	activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
}

// BeginOutput marks the start of a streaming task output section.
// The input box stays visible; output flows in the scroll region above it.
func BeginOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	streamingActive = true
	if activeReader != nil && activeReader.reading && activeReader.termH > 0 {
		// Position cursor in scroll region for streaming output.
		fmt.Printf("\x1b[%d;1H", activeReader.termH-3)
	} else if activeReader != nil && activeReader.reading {
		activeReader.eraseBox()
	}
	fmt.Print("\n")
}

// EndOutput marks the end of a streaming task output section and redraws the box.
func EndOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	streamingActive = false
	fmt.Print("\r\n")
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
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Always create a fresh buffered reader to avoid stale data from
	// previous ReadLine calls or from other stdin consumers (e.g. permission
	// prompts) that may have left the OS buffer in an inconsistent state.
	lr.rawReader = bufio.NewReader(os.Stdin)

	consoleMu.Lock()
	activeReader = lr
	lr.reading = true
	lr.renderBuf = nil
	lr.renderCursor = 0
	// Set up scroll region: pin bottom 3 rows as the input box.
	// The scroll region (rows 1 to termH-3) will receive all output, while
	// the box at rows termH-2 to termH stays fixed and is never scrolled over.
	if w, h, e := term.GetSize(int(os.Stdout.Fd())); e == nil && h > 5 {
		lr.termW = w
		lr.termH = h
		fmt.Printf("\x1b[1;%dr", h-3) // scroll region: rows 1 to h-3
		fmt.Printf("\x1b[%d;1H", h-3) // position cursor at bottom of scroll region
	} else {
		lr.termW = 0
		lr.termH = 0
	}
	consoleMu.Unlock()
	defer func() {
		consoleMu.Lock()
		if activeReader == lr {
			activeReader = nil
		}
		lr.reading = false
		if lr.termH > 0 {
			// Erase box rows and reset scroll region.
			fmt.Printf("\x1b[%d;1H\x1b[0J", lr.termH-2) // clear from box top to end
			fmt.Print("\x1b[r")                         // reset scroll region to full screen
			fmt.Printf("\x1b[%d;1H", lr.termH-3)        // position cursor after content
		}
		lr.termH = 0
		lr.termW = 0
		lr.boxDrawn = false
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
			lr.eraseBox()
			consoleMu.Unlock()
			fmt.Print("\r\n")
			return "", ErrInterrupt
		case 4:
			if len(buf) == 0 {
				consoleMu.Lock()
				lr.eraseBox()
				consoleMu.Unlock()
				fmt.Print("\r\n")
				return "", ErrExit
			}
		case '\r', '\n':
			consoleMu.Lock()
			lr.eraseBox()
			consoleMu.Unlock()
			fmt.Print("\r\n")
			line := string(buf)
			if line != "" && (len(lr.history) == 0 || lr.history[len(lr.history)-1] != line) {
				lr.history = append(lr.history, line)
			}
			lr.histIdx = len(lr.history)
			return line, nil
		case 127, 8:
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
			if err := lr.handleEscape(&buf, &cursor); err != nil {
				return "", err
			}
		case '\t':
			lr.complete(&buf, &cursor)
		default:
			if r >= 32 {
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

// eraseBox clears the input box from the screen. Must be called with consoleMu held.
func (lr *LineReader) eraseBox() {
	if lr.termH > 0 {
		// Absolute mode: move to box top-border row and clear to end of screen.
		// Cursor ends up at the scroll-region bottom row, ready for output.
		fmt.Printf("\x1b[%d;1H\x1b[0J", lr.termH-2)
		fmt.Printf("\x1b[%d;1H", lr.termH-3)
	} else if lr.boxDrawn {
		// Relative fallback: cursor is on input line; go up 1 to top border, then clear.
		fmt.Print("\x1b[A\r\x1b[0J")
	} else {
		fmt.Print("\r\x1b[0J")
	}
	lr.boxDrawn = false
}

// drawBoxAbsolute draws the 3-line input box at the fixed absolute rows
// termH-2 (top border), termH-1 (input line), termH (bottom border).
// Must be called with consoleMu held and termH > 0.
func (lr *LineReader) drawBoxAbsolute(buf []rune, cursor int) {
	w := lr.termW
	if w < 20 {
		w = 80
	}
	inner := w - 2 // visible columns between the │ chars

	maxInputVis := inner - lr.promptWidth - 2
	if maxInputVis < 1 {
		maxInputVis = 1
	}

	// Scroll window: keep cursor visible for long inputs. Width is measured in
	// terminal cells, not runes, so Chinese text and emoji don't shift cursor.
	dispBuf, cursorCells, dispCells := inputDisplayWindow(buf, cursor, maxInputVis)

	padLen := maxInputVis - dispCells
	if padLen < 0 {
		padLen = 0
	}
	pad := strings.Repeat(" ", padLen)

	topBorder := "╭" + strings.Repeat("─", inner) + "╮"
	botBorder := "╰" + strings.Repeat("─", inner) + "╯"
	coloredPrompt := " " + BrightCyan + "❯" + Reset + " "

	var inputLine string
	if len(buf) == 0 && lr.placeholder != "" {
		ph := []rune(lr.placeholder)
		ph, phCells := truncateRunesByCells(ph, maxInputVis)
		phPad := strings.Repeat(" ", maxInputVis-phCells)
		inputLine = "│" + coloredPrompt + "\x1b[90m" + string(ph) + phPad + "\x1b[0m" + " │"
	} else {
		inputLine = "│" + coloredPrompt + string(dispBuf) + pad + " │"
	}

	// Draw at absolute positions.
	fmt.Printf("\x1b[%d;1H", lr.termH-2)
	fmt.Print(topBorder)
	fmt.Printf("\x1b[%d;1H", lr.termH-1)
	fmt.Print(inputLine)
	fmt.Printf("\x1b[%d;1H", lr.termH)
	fmt.Print(botBorder)

	// Position cursor inside the input line.
	// Visible chars before input: |(1) + sp(1) + >(1) + sp(1) = 4 cols; cursor at col 5+dispCursor.
	cursorCol := 5 + cursorCells // visible cols before input: │ sp ❯ sp
	fmt.Printf("\x1b[%d;%dH", lr.termH-1, cursorCol)
	lr.boxDrawn = true
}

func (lr *LineReader) redrawLocked(buf []rune, cursor int) {
	lr.renderBuf = append(lr.renderBuf[:0], buf...)
	lr.renderCursor = cursor
	if streamingActive {
		return
	}
	if lr.termH > 0 {
		lr.drawBoxAbsolute(buf, cursor)
		return
	}
	// ── Relative fallback (non-TTY / terminal too small) ──────────────────
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 20 {
		w = 80
	}
	inner := w - 2

	lr.eraseBox()
	lr.boxDrawn = true

	maxInputVis := inner - lr.promptWidth - 2
	if maxInputVis < 1 {
		maxInputVis = 1
	}
	dispBuf, cursorCells, dispCells := inputDisplayWindow(buf, cursor, maxInputVis)
	padLen := maxInputVis - dispCells
	if padLen < 0 {
		padLen = 0
	}
	pad := strings.Repeat(" ", padLen)
	topBorder := "╭" + strings.Repeat("─", inner) + "╮"
	botBorder := "╰" + strings.Repeat("─", inner) + "╯"
	coloredPrompt := " " + BrightCyan + "❯" + Reset + " "
	var inputLine string
	if len(buf) == 0 && lr.placeholder != "" {
		ph := []rune(lr.placeholder)
		ph, phCells := truncateRunesByCells(ph, maxInputVis)
		phPad := strings.Repeat(" ", maxInputVis-phCells)
		inputLine = "│" + coloredPrompt + "\x1b[90m" + string(ph) + phPad + "\x1b[0m" + " │"
	} else {
		inputLine = "│" + coloredPrompt + string(dispBuf) + pad + " │"
	}
	fmt.Print(topBorder + "\r\n")
	fmt.Print(inputLine + "\r\n")
	fmt.Print(botBorder)
	cursorCol := 4 + cursorCells
	fmt.Print("\x1b[A\r")
	if cursorCol > 0 {
		fmt.Printf("\x1b[%dC", cursorCol)
	}
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

	if lr.termH > 0 {
		// Fixed-box mode: update the status line above the box without scrolling.
		fmt.Print("\x1b7")
		fmt.Printf("\x1b[%d;1H\x1b[2K", lr.termH-3)
		printHints()
		fmt.Print("\x1b8")
		return
	}

	if lr.boxDrawn {
		// Relative fallback: erase box, print hint line, redraw box.
		lr.eraseBox()
		printHints()
		fmt.Print("\r\n")
		lr.redrawLocked(lr.renderBuf, lr.renderCursor)
		return
	}

	// Plain inline hint (no box drawn yet).
	printHints()
	fmt.Printf("\r\x1b[%dC", cursorOffset)
}

func (lr *LineReader) showCommandCountHint(count int, cursorOffset int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	printHint := func() {
		fmt.Printf("\x1b[90m  (按 Tab 查看 %d 个命令)\x1b[0m", count)
	}

	if lr.termH > 0 {
		fmt.Print("\x1b7")
		fmt.Printf("\x1b[%d;1H\x1b[2K", lr.termH-3)
		printHint()
		fmt.Print("\x1b8")
		return
	}

	if lr.boxDrawn {
		lr.eraseBox()
		printHint()
		fmt.Print("\r\n")
		lr.redrawLocked(lr.renderBuf, lr.renderCursor)
		return
	}

	printHint()
	fmt.Printf("\r\x1b[%dC", cursorOffset)
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
	if lr.completer == nil {
		return
	}
	line := string(*buf)
	suggestions := lr.completer(line)
	if len(suggestions) == 0 {
		return
	}
	// Extract command text (before \t if annotated)
	texts := make([]string, len(suggestions))
	for i, s := range suggestions {
		if idx := strings.IndexByte(s, '\t'); idx >= 0 {
			texts[i] = s[:idx]
		} else {
			texts[i] = s
		}
	}
	if len(suggestions) == 1 {
		*buf = []rune(texts[0])
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
		return
	}
	common := commonPrefix(texts)
	if len(common) > len(line) {
		*buf = []rune(common)
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
		return
	}
	// Multiple matches without a common prefix: show a compact, box-aware hint
	// instead of dumping a persistent list into the scrollback on every Tab.
	lr.showInlineSuggestions(suggestions, lr.promptWidth+*cursor)
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
