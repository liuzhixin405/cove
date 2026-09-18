package textutil

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Runes whose Unicode East Asian Width really is Ambiguous: one column in a
// Western locale, two in a CJK one.
var ambiguous = []string{"·", "…", "↑"}

// Runes the standard calls Neutral — nominally one column — that terminals
// nevertheless often draw two cells wide when a CJK font supplies a full-width
// glyph. Windows Terminal does exactly that, which is how a row computed as
// exactly the terminal width ended up one column over and clipped its
// right-aligned element ("/x r1" reaching the screen as "/x r").
//
// There is no table that settles this, which is the point: the layout must not
// depend on their width at all. Nothing is right-aligned any more, and the
// frame keeps a spare column, so being wrong about these costs a column of
// whitespace instead of a wrapped row.
var neutralButOftenWide = []string{"▸", "▾", "✓", "✗", "⎿", "›", "⚙"}

func TestAmbiguousRunesAreMeasuredWide(t *testing.T) {
	// The conservative reading for the class the flag does cover. Measuring
	// these narrow is the dangerous direction: a row computed as exactly the
	// terminal width is then really one column over, the terminal soft-wraps
	// it, and the frame silently gains a row it did not budget for.
	for _, g := range ambiguous {
		if got := Width(g); got != 2 {
			t.Errorf("%q measures %d columns, want 2 (the conservative reading)", g, got)
		}
	}
}

func TestNeutralSymbolsAreDocumentedAsUnreliable(t *testing.T) {
	// Not a behaviour we want, just one we have pinned: the flag does not
	// reach these, so callers must not rely on their width. If a future
	// version of the width data starts reporting 2, that is an improvement and
	// this test says so out loud rather than failing somewhere subtle.
	for _, g := range neutralButOftenWide {
		if got := Width(g); got != 1 && got != 2 {
			t.Errorf("%q measures %d columns, expected 1 or 2", g, got)
		}
	}
}

func TestWidthIsAtLeastTheNarrowReading(t *testing.T) {
	// Over-estimating leaves a harmless gap; under-estimating wraps. So this
	// measure must never come out below the one it replaces.
	samples := []string{
		"plain ascii",
		"帮我把 glob 的 ** 支持修好",
		"  ▸ bash  go test ./...",
		"    ⎿ ✓ ok 1.9s · 共 12 行",
		"\x1b[96m▸ 思考\x1b[0m 172ms",
		strings.Join(ambiguous, ""),
	}
	for _, s := range samples {
		if got, narrow := Width(s), ansi.StringWidth(s); got < narrow {
			t.Errorf("%q measures %d, below the narrow reading %d", s, got, narrow)
		}
	}
}

func TestWidthIgnoresEscapeSequences(t *testing.T) {
	plain := "▸ 思考 172ms"
	styled := "\x1b[3;38;5;243m▸ 思考 172ms\x1b[0m"
	if Width(plain) != Width(styled) {
		t.Fatalf("styling changed the measured width: %d vs %d", Width(plain), Width(styled))
	}
}

func TestTruncateWidthAgreesWithWidth(t *testing.T) {
	// A truncation that disagrees with the width check it was meant to satisfy
	// just moves the off-by-one somewhere else.
	samples := []string{
		"  ▸ bash  go test ./internal/tool/ -run TestGlobDoubleStar -count=1",
		"    ⎿ ✗ --- FAIL: TestGlobDoubleStar · 共 3 行",
		"帮我把 glob 的 ** 支持修好，并补上跨目录匹配的测试用例",
		strings.Repeat("▸", 50),
	}
	for _, s := range samples {
		for _, w := range []int{1, 2, 5, 10, 20, 40, 80} {
			for _, tail := range []string{"", "…"} {
				got := TruncateWidth(s, w, tail)
				if n := Width(got); n > w {
					t.Errorf("TruncateWidth(%q, %d, %q) is %d columns wide", s, w, tail, n)
				}
			}
		}
	}
}

func TestTruncateWidthKeepsShortStringsIntact(t *testing.T) {
	s := "  ▸ bash"
	if got := TruncateWidth(s, 80, "…"); got != s {
		t.Fatalf("a string that fits was altered: %q -> %q", s, got)
	}
}

func TestTruncateWidthDropsATailThatWouldNotFit(t *testing.T) {
	// "…" is two columns under the conservative reading, so a one-column
	// budget cannot hold it. Keeping it would return a result wider than the
	// budget the caller asked for.
	if got := TruncateWidth("abcdef", 1, "…"); Width(got) > 1 {
		t.Fatalf("one-column truncation returned %q (%d columns)", got, Width(got))
	}
}

func TestTruncateWidthHandlesDegenerateWidths(t *testing.T) {
	for _, w := range []int{-5, 0} {
		if got := TruncateWidth("▸ 思考", w, "…"); got != "" {
			t.Errorf("width %d returned %q, want empty", w, got)
		}
	}
}
