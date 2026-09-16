package termui

import (
	"strings"
	"testing"
)

func TestNormalizeOutputNewlines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare lf becomes crlf", "a\nb", "a\r\nb"},
		{"existing crlf is not doubled", "a\r\nb", "a\r\nb"},
		{"mixed input is unified", "a\r\nb\nc", "a\r\nb\r\nc"},
		{"lone cr is left alone", "progress\rdone", "progress\rdone"},
		{"blank lines", "a\n\nb", "a\r\n\r\nb"},
		{"trailing newline", "a\n", "a\r\n"},
		{"leading newline", "\na", "\r\na"},
		{"no newlines", "plain", "plain"},
		{"empty", "", ""},
		{"cjk is untouched", "第一行\n第二行", "第一行\r\n第二行"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeOutputNewlines(tc.in); got != tc.want {
				t.Errorf("normalizeOutputNewlines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeOutputNewlinesIsIdempotent is the invariant that keeps output
// from gaining a blank line every time it passes through a printer: applying
// the conversion twice must equal applying it once.
func TestNormalizeOutputNewlinesIsIdempotent(t *testing.T) {
	inputs := []string{
		"a\nb", "a\r\nb", "a\r\r\nb", "\n\n", "a\rb\nc\r\nd",
		"中文\n混合\r\n输出", "", "\r", "\n",
	}
	for _, in := range inputs {
		once := normalizeOutputNewlines(in)
		twice := normalizeOutputNewlines(once)
		if once != twice {
			t.Errorf("not idempotent for %q: once = %q, twice = %q", in, once, twice)
		}
	}
}

func TestPrintAboveConvertsNewlinesForRawMode(t *testing.T) {
	c := captureStdout(t)
	PrintAbove("line one\nline two\n")
	got := c.finish()

	if want := "line one\r\nline two\r\n"; got != want {
		t.Errorf("PrintAbove wrote %q, want %q", got, want)
	}
}

func TestPrintSafeFormatsThenNormalizes(t *testing.T) {
	c := captureStdout(t)
	PrintSafe("%s: %d 个文件\n更新完成\n", "结果", 3)
	got := c.finish()

	if want := "结果: 3 个文件\r\n更新完成\r\n"; got != want {
		t.Errorf("PrintSafe wrote %q, want %q", got, want)
	}
}

func TestStreamPrintNormalizesWithoutAddingAnything(t *testing.T) {
	c := captureStdout(t)
	StreamPrint("chunk")
	StreamPrint(" more\n")
	got := c.finish()

	if want := "chunk more\r\n"; got != want {
		t.Errorf("StreamPrint wrote %q, want %q", got, want)
	}
}

// TestPrintTransientStatusResetsStateBeforeWriting pins the exact prologue:
// SGR reset (so a half-written color from the previous frame cannot bleed),
// show-cursor, carriage return, and erase-to-end-of-line. Dropping any one of
// them leaves visible artifacts on the status line.
func TestPrintTransientStatusResetsStateBeforeWriting(t *testing.T) {
	c := captureStdout(t)
	PrintTransientStatus("  working")
	got := c.finish()

	if want := "\x1b[0m\x1b[?25h\r\x1b[K  working"; got != want {
		t.Errorf("PrintTransientStatus wrote %q, want %q", got, want)
	}
}

func TestPrintTransientStatusEmptyStillClearsTheLine(t *testing.T) {
	// This is how the indicators erase themselves on Stop: the prologue must be
	// emitted even with nothing to draw.
	c := captureStdout(t)
	PrintTransientStatus("")
	got := c.finish()

	if want := "\x1b[0m\x1b[?25h\r\x1b[K"; got != want {
		t.Errorf("PrintTransientStatus(\"\") wrote %q, want %q", got, want)
	}
}

// TestPrintTransientStatusDoesNotNormalizeNewlines documents a deliberate
// difference from the other printers: the status line is a single line redrawn
// in place, so it must not run through the CRLF conversion.
func TestPrintTransientStatusDoesNotNormalizeNewlines(t *testing.T) {
	c := captureStdout(t)
	PrintTransientStatus("a\nb")
	got := c.finish()

	if strings.Contains(strings.TrimPrefix(got, "\x1b[0m\x1b[?25h\r\x1b[K"), "\r\n") {
		t.Errorf("transient status normalized its newlines: %q", got)
	}
}

func TestBeginAndEndOutput(t *testing.T) {
	c := captureStdout(t)
	BeginOutput()
	EndOutput()
	got := c.finish()

	if want := "\n\r\n"; got != want {
		t.Errorf("BeginOutput+EndOutput wrote %q, want %q", got, want)
	}
}
