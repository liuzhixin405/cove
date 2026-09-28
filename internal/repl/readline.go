package repl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
	lr.promptWidth = promptVisibleWidth(p)
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
	pinIfStreaming()

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
			// 流式输出进行中：输入行钉在底部时，把内容回显到上方的输出流里（先另起一行，
			// 不接在模型未完成的句子后面），让排队的指令在记录里可见；没有钉住时不回显，
			// 否则会把提示符+内容插进流式文本里造成错乱。
			switch {
			case !streamingActive:
				fmt.Print(lr.prompt + normalizeOutputNewlines(line) + "\r\n")
			case pinned && line != "":
				// An empty Enter is ignored by the main loop, so it leaves
				// no trace here either; it used to print a bare prompt row
				// into the model's output at every press.
				breakStreamLineLocked()
				fmt.Print(PromptRunning() + normalizeOutputNewlines(line) + "\r\n")
			}
			consoleMu.Unlock()

			if line != "" && (len(lr.history) == 0 || lr.history[len(lr.history)-1] != line) {
				lr.history = append(lr.history, line)
			}
			lr.histIdx = len(lr.history)
			return line, nil
		case 12:
			// Ctrl+L clears the screen and keeps what is being typed.
			consoleMu.Lock()
			lr.renderBuf = append(lr.renderBuf[:0], buf...)
			lr.renderCursor = cursor
			consoleMu.Unlock()
			ClearScreen()
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
	if !errors.Is(err, io.EOF) {
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

func (lr *LineReader) redraw(buf []rune, cursor int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.activeHint = ""
	lr.redrawLocked(buf, cursor)
}

func (lr *LineReader) eraseLineLocked() {
	// While streaming, the input line is not on the current terminal line
	// (it is off screen or pinned to the last row), so that line holds
	// streamed output; erasing it would corrupt the stream.
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
		if pinned {
			lr.drawPinnedLocked()
		}
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
