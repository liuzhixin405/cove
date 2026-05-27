package repl

import (
	"bufio"
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
	promptWidth    int // visible character width (excluding ANSI codes)
	placeholder    string
	fallbackReader *bufio.Reader
	rawReader      *bufio.Reader
}

var ErrExit = fmt.Errorf("exit")
var ErrInterrupt = fmt.Errorf("interrupt")

func New(completer Completer) *LineReader {
	return &LineReader{
		completer:   completer,
		prompt:      Prompt(),
		promptWidth: 2, // "❯ " visible chars
		placeholder: "(press / to show commands)",
	}
}

func PrintSafe(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	s = strings.ReplaceAll(s, "\n", "\r\n")
	fmt.Print(s)
}

func (lr *LineReader) ReadLine() (string, error) {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return lr.fallbackRead()
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	if lr.rawReader == nil {
		lr.rawReader = bufio.NewReader(os.Stdin)
	}

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
			fmt.Print("\r\n")
			return "", ErrInterrupt
		case 4:
			if len(buf) == 0 {
				fmt.Print("\r\n")
				return "", ErrExit
			}
		case '\r', '\n':
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
					} else if line == "/" && len(suggestions) > 10 {
						fmt.Print("  \x1b[90m(press Tab to list available commands)\x1b[0m")
						fmt.Printf("\r\x1b[%dC", lr.promptWidth+cursor)
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
					} else if line == "/" && len(suggestions) > 10 {
						fmt.Print("  \x1b[90m(press Tab to list available commands)\x1b[0m")
						fmt.Printf("\r\x1b[%dC", lr.promptWidth+cursor)
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

func (lr *LineReader) handleEscape(buf *[]rune, cursor *int) error {
	first, err := readInputRune(lr.rawReader)
	if err != nil {
		return err
	}
	if first != '[' {
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
	fmt.Print("\r\x1b[K" + lr.prompt + string(buf))
	if len(buf) == 0 && lr.placeholder != "" {
		fmt.Print(" \x1b[90m" + lr.placeholder + "\x1b[0m")
		fmt.Printf("\r\x1b[%dC", lr.promptWidth) // Move cursor back to the end of prompt
	} else if cursor < len(buf) {
		fmt.Printf("\r\x1b[%dC", lr.promptWidth+cursor)
	}
}

func (lr *LineReader) showInlineSuggestions(suggestions []string, cursorOffset int) {
	fmt.Print("  \x1b[90m")
	for _, s := range suggestions {
		fmt.Print(s + "  ")
	}
	fmt.Print("\x1b[0m")
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
	if len(suggestions) == 1 {
		*buf = []rune(suggestions[0])
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
		return
	}
	common := commonPrefix(suggestions)
	if len(common) > len(line) {
		*buf = []rune(common)
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
		return
	}
	fmt.Print("\r\n")
	for _, s := range suggestions {
		fmt.Printf("  %s\r\n", s)
	}
	lr.redraw(*buf, *cursor)
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
