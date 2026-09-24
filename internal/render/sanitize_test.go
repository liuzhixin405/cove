package render

import (
	"strings"
	"testing"
)

// Untrusted text (a file's first line, a web page, a command the model wrote)
// reached the terminal verbatim, so an escape sequence inside it was executed
// by the terminal rather than shown. These are the sequences that matter: each
// one either changes something outside the text's own row or makes the
// terminal write back into cove's stdin.
var hostileSequences = map[string]string{
	"window title (OSC 0)":       "\x1b]0;pwned\x07",
	"clipboard write (OSC 52)":   "\x1b]52;c;cm0gLXJmIH4=\x1b\\",
	"hyperlink (OSC 8)":          "\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\",
	"cursor up":                  "\x1b[3A",
	"cursor position":            "\x1b[1;1H",
	"clear screen":               "\x1b[2J",
	"status report query":        "\x1b[6n",
	"private mode (alt screen)":  "\x1b[?1049h",
	"full reset (RIS)":           "\x1bc",
	"save cursor (DECSC)":        "\x1b7",
	"charset switch":             "\x1b(0",
	"device control string":      "\x1bPq#0;2;0;0;0\x1b\\",
	"8-bit CSI (C1)":             "\u009b2J",
	"8-bit OSC (C1)":             "\u009d0;pwned\u0007",
	"bell":                       "\a",
	"backspace overwrite":        "rm\b\bls",
	"application program string": "\x1b_payload\x1b\\",
}

func TestStripControlsRemovesEverySequence(t *testing.T) {
	for name, seq := range hostileSequences {
		t.Run(name, func(t *testing.T) {
			got := StripControls("a" + seq + "b")
			if strings.ContainsAny(got, "\x1b\a\b\u009b\u009d") {
				t.Fatalf("StripControls(%q) = %q still carries a control character", seq, got)
			}
			if !strings.HasPrefix(got, "a") || !strings.HasSuffix(got, "b") {
				t.Fatalf("StripControls(%q) = %q lost the surrounding text", seq, got)
			}
		})
	}
}

func TestStripControlsRemovesColourAndCarriageReturn(t *testing.T) {
	// A header or a prompt description is one logical line that the renderer
	// styles itself, so colour from the source and a bare \r (which returns
	// to column 0 and lets later text overwrite earlier text) both go.
	if got := StripControls("\x1b[31mred\x1b[0m"); got != "red" {
		t.Fatalf("got %q, want %q", got, "red")
	}
	if got := StripControls("rm -rf ~\rls -la  "); strings.Contains(got, "\r") {
		t.Fatalf("carriage return survived: %q", got)
	}
}

func TestStripControlsKeepsTextNewlinesAndTabs(t *testing.T) {
	in := "第一行\n\tsecond ✓ line"
	if got := StripControls(in); got != in {
		t.Fatalf("StripControls(%q) = %q, want it unchanged", in, got)
	}
}

func TestSanitizeStreamDropsHostileSequences(t *testing.T) {
	for name, seq := range hostileSequences {
		t.Run(name, func(t *testing.T) {
			got := SanitizeStream("a" + seq + "b")
			if strings.ContainsAny(got, "\a\b\u009b\u009d") {
				t.Fatalf("SanitizeStream(%q) = %q still carries a control character", seq, got)
			}
			if i := strings.IndexByte(got, 0x1b); i >= 0 {
				t.Fatalf("SanitizeStream(%q) = %q still carries an escape", seq, got)
			}
		})
	}
}

func TestSanitizeStreamKeepsColourAndLineControl(t *testing.T) {
	// Tool output is legitimately coloured (go test, git, npm) and progress
	// bars redraw their own row with \r and erase-in-line; none of that can
	// leave the row it is on.
	in := "\x1b[32mok\x1b[0m  pkg\r\x1b[Kdone\n\x1b[1;31mFAIL\x1b[m\t1.2s"
	if got := SanitizeStream(in); got != in {
		t.Fatalf("SanitizeStream(%q) = %q, want it unchanged", in, got)
	}
}

func TestSanitizeStreamDropsAnIncompleteTrailingSequence(t *testing.T) {
	if got := SanitizeStream("text\x1b]0;half a tit"); got != "text" {
		t.Fatalf("got %q, want %q", got, "text")
	}
}

func TestStreamSanitizerJoinsASequenceSplitAcrossChunks(t *testing.T) {
	// Deltas and tool-progress chunks are cut wherever the transport cut
	// them, so a colour code can straddle two chunks. Dropping the first half
	// would print the second half ("1m") as literal text.
	var z StreamSanitizer
	got := z.Write("go test \x1b[3") + z.Write("1mFAIL\x1b[0m")
	if want := "go test \x1b[31mFAIL\x1b[0m"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStreamSanitizerStillDropsAHostileSequenceSplitAcrossChunks(t *testing.T) {
	var z StreamSanitizer
	got := z.Write("see \x1b]0;pw") + z.Write("ned\x07 here")
	if strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("split OSC reached the terminal: %q", got)
	}
	if !strings.HasPrefix(got, "see ") || !strings.HasSuffix(got, " here") {
		t.Fatalf("surrounding text lost: %q", got)
	}
}

func TestStreamSanitizerDoesNotHoldBackUnboundedInput(t *testing.T) {
	// An unterminated OSC must not swallow the rest of the stream forever.
	var z StreamSanitizer
	z.Write("\x1b]52;c;")
	got := z.Write(strings.Repeat("A", 8192) + " visible")
	if !strings.Contains(got, "visible") {
		t.Fatalf("text after an unterminated sequence never came out (len %d)", len(got))
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("escape leaked: %q", got[:40])
	}
}
