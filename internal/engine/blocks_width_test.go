package engine

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/textutil"
)

func withTerminalWidth(t *testing.T, w int) {
	t.Helper()
	old := terminalWidth
	terminalWidth = func() int { return w }
	t.Cleanup(func() { terminalWidth = old })
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// Tool blocks for OnEngineOutput front ends were always laid out for 120
// columns, so on an 80-column terminal every header wrapped.
func TestBlockRenderWidthFollowsTerminal(t *testing.T) {
	withTerminalWidth(t, 80)
	// One column short of the terminal: writing the last column makes the
	// Windows console wrap the line by itself.
	if got := blockRenderWidth(); got != 79 {
		t.Fatalf("blockRenderWidth() = %d, want 79 on an 80-column terminal", got)
	}

	eng := newTestEngine(&mockProvider{})
	var out strings.Builder
	eng.OnEngineOutput = func(s string) { out.WriteString(s) }
	eng.emitToolResult("bash", map[string]any{"command": strings.Repeat("echo long-argument ", 20)}, "ok", false, time.Millisecond)
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if w := textutil.Width(ansiRe.ReplaceAllString(line, "")); w > 79 {
			t.Fatalf("rendered line is %d columns on an 80-column terminal: %q", w, line)
		}
	}
}

// No terminal (piped output, a test) or an implausibly narrow one keeps the
// old 120-column layout.
func TestBlockRenderWidthFallsBackTo120(t *testing.T) {
	for _, w := range []int{0, 39, -1} {
		withTerminalWidth(t, w)
		if got := blockRenderWidth(); got != 120 {
			t.Fatalf("terminal width %d: blockRenderWidth() = %d, want 120", w, got)
		}
	}
}
