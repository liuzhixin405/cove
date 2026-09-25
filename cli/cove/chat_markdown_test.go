package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A reply cut off inside a code fence (truncated, or failed and retried) left
// the Markdown renderer in the code block, so every later answer was drawn as
// code. Each attempt starts from a fresh renderer.
func TestBeginAttemptResetsMarkdownState(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.delta("```go\nx")
	p.beginAttempt()
	p.delta("普通文本\n")
	p.stop()

	out := ansi.Strip(buf.String())
	row, ok := lineBefore(out, "普通文本")
	if !ok {
		t.Fatalf("text missing: %q", out)
	}
	if strings.Contains(row, "│") || strings.Contains(row, "|") {
		t.Fatalf("text after a new attempt is still drawn as code: row %q in %q", row, out)
	}
}

// A reply that is nothing but a held-back marker never printed anything, so
// the printer never switched to text; stop must still flush it.
func TestStopFlushesHeldBackText(t *testing.T) {
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.delta("*")
	p.stop()
	if !strings.Contains(buf.String(), "*") {
		t.Fatalf("held-back text was dropped at the end of the turn: %q", buf.String())
	}
}
