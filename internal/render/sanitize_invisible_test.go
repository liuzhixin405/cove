package render

import (
	"strings"
	"testing"
)

// u builds a string from code points, so the invisible characters under test
// are spelled out in the source instead of hiding in it.
func u(rs ...rune) string { return string(rs) }

const (
	zwsp  = 0x200b
	zwj   = 0x200d
	vs16  = 0xfe0f
	man   = 0x1f468
	woman = 0x1f469
	girl  = 0x1f467
)

// Zero-width and other invisible characters were shown raw in the approval
// prompt: "echo hi <U+200B>#; rm -rf ~" displays as `echo hi #; rm -rf ~`,
// which reads as a comment, while bash runs rm (after U+200B the "#" is not
// at the start of a word, so it starts no comment).
func TestVisibleControlsShowsInvisibleCharacters(t *testing.T) {
	for _, r := range []rune{zwsp, 0x200c, zwj, 0x2060, 0xfeff, 0x00ad, 0x180e, 0x034f, 0xe0001, 0xe0041, 0xe007f} {
		in := "echo hi " + u(r) + "#; rm -rf ~"
		got := VisibleControls(in)
		if strings.ContainsRune(got, r) {
			t.Errorf("U+%04X shown raw: %q", r, got)
		}
		if !strings.Contains(got, `\u`) || !strings.Contains(got, "#; rm -rf ~") {
			t.Errorf("U+%04X not rendered visibly: %q", r, got)
		}
	}
	if got, want := VisibleControls("a"+u(zwsp)+"b"), `a\`+"u200bb"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A ZWJ joining two emoji is part of the emoji and stays, so emoji
	// sequences still display; a ZWJ anywhere else does not.
	for _, s := range []string{
		u(man, zwj, woman, zwj, girl),  // family
		u(man, 0x1f3fd, zwj, 0x1f4bb),  // technologist, skin tone
		u(0x1f3f3, vs16, zwj, 0x1f308), // rainbow flag
		u(0x2764, vs16, zwj, 0x1f525),  // heart on fire
	} {
		if got := VisibleControls("ok " + s); got != "ok "+s {
			t.Errorf("emoji sequence %q changed: %q", s, got)
		}
	}
	for _, s := range []string{u('a', zwj, 'b'), u(man, zwj, '#'), u('#', zwj, woman), u(zwj, woman), u(man, zwj)} {
		if got := VisibleControls(s); strings.ContainsRune(got, zwj) {
			t.Errorf("stray ZWJ in %q shown raw: %q", s, got)
		}
	}
}

// OSC and DCS strings may end with the 8-bit ST (0x9C, or its UTF-8 form
// C2 9C). It was not recognised, so such a sequence swallowed up to 256
// bytes of the text after it.
func TestEightBitStringTerminatorEndsOSC(t *testing.T) {
	for _, st := range []string{"\x9c", "\xc2\x9c"} {
		for _, in := range []string{"\x1b]0;title" + st + "visible", "\x1bPq;x" + st + "visible"} {
			if got := StripControls(in); got != "visible" {
				t.Errorf("StripControls(%q) = %q, want %q", in, got, "visible")
			}
			if got := SanitizeStream(in); got != "visible" {
				t.Errorf("SanitizeStream(%q) = %q, want %q", in, got, "visible")
			}
		}
	}
}

// A 0x9C that is the last byte of a multi-byte character in the body
// (U+4F1C is E4 BC 9C) is not a terminator.
func TestEightBitSTIgnoresContinuationBytes(t *testing.T) {
	if got := StripControls("\x1b]0;" + u(0x4f1c) + "title\x07visible"); got != "visible" {
		t.Errorf("got %q, want %q", got, "visible")
	}
}
