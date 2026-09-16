package main

import (
	"strings"
	"testing"
)

// TestCleanANSIRemovesControlSequences is the regression test for the TUI
// layout falling apart as a task ran.
//
// Engine diagnostics carry terminal control sequences. Any that survive into
// the viewport are executed by the terminal behind the renderer's back, so the
// terminal and the renderer drift apart and never resync. The old
// implementation cut from "ESC[" to the next 'm' ANYWHERE in the remaining
// text, which both destroyed content and left non-SGR sequences in place.
func TestCleanANSIRemovesControlSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"sgr color", "\x1b[36mread\x1b[0m file.go", "read file.go"},
		{"erase line with cr", "\r\x1b[K  ✓ read file.go\n", "  ✓ read file.go\n"},
		// The killer case: a non-SGR sequence followed by text containing 'm'.
		// The old loop swallowed everything up to "m" in "command".
		{"cursor up then m-text", "\x1b[1Arunning command now", "running command now"},
		{"clear screen", "\x1b[2Jhello memory", "hello memory"},
		{"hide cursor", "\x1b[?25lmodel ready", "model ready"},
		// No 'm' anywhere after the escape: the old loop gave up and left it in.
		{"no trailing m", "\x1b[2Kabcdef", "abcdef"},
		{"osc title", "\x1b]0;cove\x07done", "done"},
		{"bare bell", "beep\x07 done", "beep done"},
		{"tabs and newlines kept", "a\tb\nc", "a\tb\nc"},
		{"cjk untouched", "更新配置文件 \x1b[1m完成\x1b[0m", "更新配置文件 完成"},
		{"plain passthrough", "nothing to strip", "nothing to strip"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanANSI(tc.in)
			if got != tc.want {
				t.Fatalf("cleanANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCleanANSILeavesNoControlBytes asserts the invariant the TUI depends on:
// nothing that can steer the terminal reaches the viewport.
func TestCleanANSILeavesNoControlBytes(t *testing.T) {
	noisy := "\x1b[36m工具\x1b[0m \r\x1b[K \x1b[1A \x1b[?25h \x1b]8;;http://x\x07link\x1b]8;;\x07 \x00\x08 ok"
	got := cleanANSI(noisy)
	for _, r := range got {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			t.Fatalf("cleanANSI left control byte %#x in %q", r, got)
		}
	}
	if !strings.Contains(got, "工具") || !strings.Contains(got, "ok") {
		t.Fatalf("cleanANSI dropped real content: %q", got)
	}
}
