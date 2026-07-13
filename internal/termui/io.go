package termui

import (
	"fmt"
	"strings"
	"sync"
)

var consoleMu sync.Mutex

func normalizeOutputNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func PrintSafe(format string, args ...any) {
	PrintAbove(fmt.Sprintf(format, args...))
}

func PrintAbove(s string) {
	s = normalizeOutputNewlines(s)
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Print(s)
}

func StreamPrint(s string) {
	s = normalizeOutputNewlines(s)
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Print(s)
}

func PrintTransientStatus(s string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Print("\x1b[0m\x1b[?25h\r\x1b[K" + s)
}

func BeginOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Print("\n")
}

func EndOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Print("\r\n")
}
