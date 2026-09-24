package repl

import (
	"bufio"
	"strings"
	"testing"
)

// withActiveReader registers lr as the editor on screen, the state ReadLine
// is in while a task streams.
func withActiveReader(t *testing.T, lr *LineReader) {
	t.Helper()
	consoleMu.Lock()
	activeReader = lr
	lr.reading = true
	consoleMu.Unlock()
	t.Cleanup(func() {
		consoleMu.Lock()
		activeReader = nil
		lr.reading = false
		streamingActive = false
		consoleMu.Unlock()
	})
}

// A permission prompt that arrived while the stream had left a partial line
// (reasoning or a tool's output without a trailing newline) redrew the input
// line with \r ESC[2K on that same row, erasing the partial text.
func TestPromptInputDoesNotEraseAPartialStreamedLine(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	lr.rawReader = bufio.NewReader(strings.NewReader(""))
	withActiveReader(t, lr)

	BeginOutput()
	StreamPrint("partial reasoning without newline")
	BeginPromptInput()
	EndPromptInput()
	EndOutput()
	out := restore()

	i := strings.Index(out, "partial reasoning without newline")
	if i < 0 {
		t.Fatalf("streamed text missing: %q", out)
	}
	rest := out[i+len("partial reasoning without newline"):]
	erase := strings.Index(rest, "\x1b[2K")
	nl := strings.Index(rest, "\r\n")
	if erase >= 0 && (nl < 0 || erase < nl) {
		t.Fatalf("the prompt's redraw erased the partial line: %q", rest)
	}
}

// A notice printed mid-stream ("[任务排队中] …" after the user typed ahead) was
// glued to the end of the model's unfinished sentence.
func TestPrintAboveDuringStreamingStartsOnItsOwnLine(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withActiveReader(t, lr)

	BeginOutput()
	StreamPrint("the model is still typing")
	PrintAbove("[任务排队中] 前方排队数: 1\n")
	EndOutput()
	out := restore()

	if strings.Contains(out, "typing[任务排队中]") {
		t.Fatalf("notice glued to the streamed text: %q", out)
	}
}

func TestPromptInputAfterACompleteLineAddsNoBlankLine(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withActiveReader(t, lr)

	BeginOutput()
	StreamPrint("whole line\n")
	BeginPromptInput()
	EndPromptInput()
	EndOutput()
	out := restore()

	if strings.Contains(out, "whole line\r\n\r\n\x1b") {
		t.Fatalf("an extra blank line was added before the prompt: %q", out)
	}
}
