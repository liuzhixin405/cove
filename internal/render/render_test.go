package render

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// lines splits a rendered block into rows.
// uni is the default glyph set, named so the assertions read as "the glyph the
// renderer should have used" rather than repeating the literal characters.
var uni = UnicodeGlyphs()

func lines(s string) []string { return strings.Split(s, "\n") }

// assertFits is the invariant every caller depends on: no rendered row may
// exceed the terminal width, because a row one column too wide soft-wraps and
// silently adds a line to the frame.
func assertFits(t *testing.T, out string, width int) {
	t.Helper()
	for i, l := range lines(out) {
		if w := textutil.Width(l); w > width {
			t.Errorf("row %d is %d columns wide, terminal is %d: %q", i, w, width, l)
		}
		if !utf8.ValidString(l) {
			t.Errorf("row %d is not valid UTF-8: %q", i, l)
		}
	}
}

// ---------------------------------------------------------------------------
// Collapsed tool rendering
// ---------------------------------------------------------------------------

func TestCollapsedToolIsTwoLines(t *testing.T) {
	b := ToolBlock("a4", "bash", "go test ./internal/tool/", "", "ok\t0.336s\nPASS\nmore\n", false, 336*time.Millisecond)
	out := Collapsed(b, 80, Styles{})
	got := lines(out)

	if len(got) != 2 {
		t.Fatalf("collapsed tool rendered %d lines, want 2:\n%s", len(got), out)
	}
	if !strings.Contains(got[0], "bash") || !strings.Contains(got[0], "go test") {
		t.Errorf("header line lost its content: %q", got[0])
	}
	if !strings.Contains(got[1], uni.Cont) {
		t.Errorf("summary line is missing the %q continuation glyph: %q", uni.Cont, got[1])
	}
	assertFits(t, out, 80)
}

// TestCollapsedToolDistinguishesSuccessAndFailure is the regression test for
// engine's formatToolLine, which printed a literal "?" for BOTH outcomes and
// left color as the only signal — invisible on a low-contrast terminal or to a
// colorblind user.
func TestCollapsedToolDistinguishesSuccessAndFailure(t *testing.T) {
	ok := Collapsed(ToolBlock("a1", "bash", "go build", "", "ok", false, 0), 80, Styles{})
	bad := Collapsed(ToolBlock("a2", "bash", "go build", "", "compile error", true, 0), 80, Styles{})

	if !strings.Contains(ok, uni.OK) {
		t.Errorf("success block has no %q glyph:\n%s", uni.OK, ok)
	}
	if !strings.Contains(bad, uni.Err) {
		t.Errorf("failure block has no %q glyph:\n%s", uni.Err, bad)
	}
	if strings.Contains(ok, uni.Err) {
		t.Errorf("success block shows the failure glyph:\n%s", ok)
	}
	// The two must differ in PLAIN text, not only in styling.
	if ansi.Strip(ok) == ansi.Strip(bad) {
		t.Error("success and failure render identically once color is stripped")
	}
}

// TestCollapsedAlignsOnOneGutter pins the property that fixes the "looks
// messy" complaint: every kind starts its text at the same column.
//
// The layout is gutter, then exactly one cell of disclosure marker, then one
// space, then content. A block with nothing hidden leaves the marker cell
// blank rather than dropping it, which is what keeps its name lined up with
// its expandable neighbours instead of sitting one column left of them.
func TestCollapsedAlignsOnOneGutter(t *testing.T) {
	blocks := []Block{
		UserBlock("帮我把 glob 的 ** 支持修好"),
		ThinkingBlock("t1", "reasoning body", 12*time.Second, 1200),
		ToolBlock("a1", "grep", `"filepath.Match" internal/tool`, "3 matches", "a\nb\nc", false, 0),
		// read is summary-only, so this one has nothing to disclose.
		ToolBlock("a2", "read", "internal/tool/glob.go", "78 lines", "78 lines", false, 0),
	}
	for _, st := range []Styles{{}, {Glyphs: ASCIIGlyphs()}} {
		for _, b := range blocks {
			first := []rune(ansi.Strip(lines(Collapsed(b, 80, st))[0]))
			if len(first) < 5 {
				t.Fatalf("kind %d header is too short to align: %q", b.Kind, string(first))
			}
			if string(first[:2]) != gutter {
				t.Errorf("kind %d does not start at the shared gutter: %q", b.Kind, string(first))
				continue
			}
			if first[3] != ' ' {
				t.Errorf("kind %d has no separator after the disclosure column: %q", b.Kind, string(first))
			}
			if first[4] == ' ' {
				t.Errorf("kind %d indents its content past the shared column: %q", b.Kind, string(first))
			}
		}
	}
}

// TestDisclosureMarkerReflectsState is the affordance contract: a row that can
// be opened says so, a row that is open says that instead, and a row with
// nothing behind it makes no promise at all.
func TestDisclosureMarkerReflectsState(t *testing.T) {
	g := UnicodeGlyphs()
	expandable := ToolBlock("a1", "bash", "go test ./...", "ok", "line\nline", false, 0)
	leaf := ToolBlock("a2", "read", "a.go", "78 lines", "78 lines", false, 0)

	closed := lines(Collapsed(expandable, 80, Styles{}))[0]
	if !strings.Contains(closed, g.Closed) {
		t.Errorf("an expandable block does not show the closed marker: %q", closed)
	}

	inert := lines(Collapsed(leaf, 80, Styles{}))[0]
	if strings.Contains(inert, g.Closed) || strings.Contains(inert, g.Open) {
		t.Errorf("a block with nothing hidden offers a disclosure marker: %q", inert)
	}
}

// ---------------------------------------------------------------------------
// The expand hint
// ---------------------------------------------------------------------------

// TestNoHintWhenNothingHidden covers the rule that an affordance which does
// nothing must not be shown: "read → 78 lines" has no extra content, so it
// gets no disclosure marker and no expand reference.
func TestNoHintWhenNothingHidden(t *testing.T) {
	// read is policySummaryOnly: Full is never populated.
	b := ToolBlock("a1", "read", "internal/tool/glob.go", "78 lines", "78 lines of content here", false, 0)
	if b.Expandable() {
		t.Fatal("a read block was marked expandable")
	}
	if out := Collapsed(b, 80, Styles{}); strings.Contains(out, "/x") {
		t.Errorf("hint shown for a block with nothing hidden:\n%s", out)
	}

	// A single-line result identical to its summary adds nothing either.
	b2 := ToolBlock("a2", "grep", "pattern", "", "no matches", false, 0)
	if b2.Expandable() {
		t.Errorf("a single-line result was marked expandable (Full=%q, Summary=%q)", b2.Full, b2.Summary)
	}
}

// TestNothingIsRightAligned is the regression test for a whole class of bug.
//
// The collapsed row used to carry a right-aligned "/x <id>". Positioning it
// meant padding to the exact terminal width, which meant every row depended on
// measuring a decorative glyph correctly — and terminals disagree about those
// (Windows Terminal draws several nominally one-column symbols two cells wide).
// One column of error clipped the last character, so "/x r1" reached the screen
// as "/x r" and the command it advertised did not work.
//
// Nothing is right-aligned any more, so a row's length no longer has to be
// exact. The disclosure marker is the affordance instead.
func TestNothingIsRightAligned(t *testing.T) {
	b := ToolBlock("a4", "bash", "go test ./...", "", "line1\nline2\nline3\n", false, 0)
	if !b.Expandable() {
		t.Fatal("a multi-line bash result was not marked expandable")
	}
	out := Collapsed(b, 80, Styles{})
	if strings.Contains(out, "/x") {
		t.Errorf("a collapsed row still advertises an expand command:\n%s", out)
	}
	for i, l := range lines(out) {
		if strings.HasSuffix(l, " ") {
			t.Errorf("row %d is padded to the terminal width: %q", i, l)
		}
	}
	assertFits(t, out, 80)
}

// TestCollapsedStaysTwoLinesAtAnyWidth: a collapsed tool call is two rows, full
// stop. A third row would break the caller's line budget.
func TestCollapsedStaysTwoLinesAtAnyWidth(t *testing.T) {
	long := strings.Repeat("很长的命令", 30)
	b := ToolBlock("a9", "bash", long, "", "a\nb", false, 0)
	for _, w := range []int{20, 24, 30, 40, 80, 200} {
		out := Collapsed(b, w, Styles{})
		assertFits(t, out, w)
		if len(lines(out)) > 2 {
			t.Errorf("width=%d produced %d lines, want at most 2:\n%s", w, len(lines(out)), out)
		}
	}
}

// TestErrorIsAlwaysExpandable: whatever the tool's policy, the reason a call
// failed is exactly what the user needs next.
func TestErrorIsAlwaysExpandable(t *testing.T) {
	// read is policySummaryOnly, but this one failed.
	b := ToolBlock("a1", "read", "missing.go", "", "Error: file not found\nstack trace here", true, 0)
	if !b.Expandable() {
		t.Fatal("a failed summary-only tool call hid its error output")
	}
}

// ---------------------------------------------------------------------------
// Thinking
// ---------------------------------------------------------------------------

func TestThinkingIsAlwaysOneLine(t *testing.T) {
	body := strings.Repeat("推理内容很长很长。", 200)
	b := ThinkingBlock("t1", body, 12*time.Second, 1200)
	out := Collapsed(b, 80, Styles{})

	if n := len(lines(out)); n != 1 {
		t.Fatalf("thinking collapsed to %d lines, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "推理内容") {
		t.Errorf("thinking body leaked into the collapsed form:\n%s", out)
	}
	if !strings.Contains(out, "12.0s") || !strings.Contains(out, "1.2k") {
		t.Errorf("collapsed thinking lost its duration/token summary: %q", out)
	}
	if !b.Expandable() {
		t.Error("thinking with a body should be expandable")
	}
	assertFits(t, out, 80)
}

func TestThinkingWithoutBodyIsNotExpandable(t *testing.T) {
	if ThinkingBlock("t1", "   ", time.Second, 10).Expandable() {
		t.Error("empty reasoning was marked expandable")
	}
}

// ---------------------------------------------------------------------------
// Width and CJK
// ---------------------------------------------------------------------------

// TestEveryKindFitsEveryWidth is the broad safety net: the caller prints these
// rows straight into the terminal, so nothing may ever exceed the width.
func TestEveryKindFitsEveryWidth(t *testing.T) {
	blocks := []Block{
		UserBlock("帮我把 glob 的 ** 支持修好，并补上跨目录匹配的测试用例"),
		ThinkingBlock("t1", "body", 90*time.Second, 25000),
		ToolBlock("a1", "bash", "go test -race ./... -count=1 2>&1 | tail -40", "", "很长的中文输出\n第二行\n第三行", false, 2*time.Second),
		ToolBlock("a2", "edit", `D:\github\cove-main\cove-main\internal\tool\glob.go`, "+52 −8", "diff body", false, 0),
		ToolBlock("a3", "grep", "模式包含中文字符", "", "", true, 0),
		AnswerBlock("filepath.Match 不支持 **，因为 * 不跨路径分隔符。我实现了按段匹配的 matchGlob。"),
		SystemBlock("已恢复会话: 重构配置加载", false),
		SystemBlock("预算已超限", true),
	}
	for _, w := range []int{20, 24, 40, 60, 80, 100, 120, 200} {
		for i, b := range blocks {
			out := Collapsed(b, w, Styles{})
			assertFits(t, out, w)
			if strings.TrimSpace(ansi.Strip(out)) == "" && b.Kind != KindSystem {
				t.Errorf("width=%d block %d rendered empty", w, i)
			}
		}
	}
}

// TestNarrowWidthDoesNotPanic covers degenerate widths, which arrive during
// terminal resize before the real size is known.
func TestNarrowWidthDoesNotPanic(t *testing.T) {
	b := ToolBlock("a1", "bash", "go build ./...", "", "out\nmore", false, 0)
	for _, w := range []int{-5, 0, 1, 2, 5, 10, 19} {
		out := Collapsed(b, w, Styles{})
		if !utf8.ValidString(out) {
			t.Errorf("width=%d produced invalid UTF-8", w)
		}
	}
}

// TestStylesAreApplied confirms the injection point works and that the zero
// Styles value renders plain text (which is what makes these tests readable).
func TestStylesAreApplied(t *testing.T) {
	b := ToolBlock("a1", "bash", "go build", "", "ok\nmore", false, 0)

	plain := Collapsed(b, 80, Styles{})
	if strings.ContainsRune(plain, 0x1b) {
		t.Errorf("the zero Styles value emitted escapes: %q", plain)
	}

	styled := Collapsed(b, 80, Styles{
		ToolName: func(s string) string { return "<T>" + s + "</T>" },
		Summary:  func(s string) string { return "<S>" + s + "</S>" },
	})
	if !strings.Contains(styled, "<T>") {
		t.Errorf("ToolName style not applied:\n%s", styled)
	}
	if !strings.Contains(styled, "<S>") {
		t.Errorf("Summary style not applied:\n%s", styled)
	}
}

// ---------------------------------------------------------------------------
// Summary derivation
// ---------------------------------------------------------------------------

func TestDefaultSummaryReportsWithheldLines(t *testing.T) {
	b := ToolBlock("a1", "bash", "ls", "", "file1\nfile2\nfile3\nfile4", false, 0)
	if !strings.Contains(b.Summary, "4 行") {
		t.Errorf("summary does not say how much was withheld: %q", b.Summary)
	}
	if !strings.Contains(b.Summary, "file1") {
		t.Errorf("summary dropped the leading line, where tools put the verdict: %q", b.Summary)
	}
}

func TestDefaultSummaryHandlesEmptyOutput(t *testing.T) {
	if got := ToolBlock("a1", "bash", "true", "", "", false, 0).Summary; got != "完成" {
		t.Errorf("empty success summary = %q", got)
	}
	if got := ToolBlock("a2", "bash", "false", "", "", true, 0).Summary; !strings.Contains(got, "失败") {
		t.Errorf("empty failure summary = %q", got)
	}
}

func TestCallerSuppliedSummaryWins(t *testing.T) {
	b := ToolBlock("a1", "edit", "glob.go", "+52 −8", "a\nb\nc", false, 0)
	if b.Summary != "+52 −8" {
		t.Errorf("Summary = %q, want the caller's %q", b.Summary, "+52 −8")
	}
}

func TestSummaryIsSingleLineAndBounded(t *testing.T) {
	b := ToolBlock("a1", "bash", "x", "", strings.Repeat("很长的一行输出", 100)+"\nsecond", false, 0)
	if strings.Contains(b.Summary, "\n") {
		t.Errorf("summary contains a newline: %q", b.Summary)
	}
	if n := utf8.RuneCountInString(b.Summary); n > maxSummaryRunes {
		t.Errorf("summary is %d runes, want at most %d", n, maxSummaryRunes)
	}
	if !utf8.ValidString(b.Summary) {
		t.Error("summary is not valid UTF-8")
	}
}

func TestPolicyForUnknownToolIsExpandable(t *testing.T) {
	// A newly added tool must default to showing its output, not hiding it.
	if policyFor("some_future_tool") != policyExpandable {
		t.Error("an unknown tool defaulted to hiding its output")
	}
	if policyFor("BASH") != policyAlwaysExpandable {
		t.Error("policy lookup is case-sensitive")
	}
}

func TestOneLineCollapsesAllWhitespace(t *testing.T) {
	got := oneLine("a\r\nb\tc   d\ne")
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("oneLine left a control character: %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("oneLine left a double space: %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		336 * time.Millisecond: "336ms",
		2 * time.Second:        "2.0s",
		90 * time.Second:       "1m30s",
		3661 * time.Second:     "61m01s",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Glyph sets
// ---------------------------------------------------------------------------

// TestASCIIGlyphsStayDistinguishable is the reason the set is swappable at
// all. On a console that cannot render ✓/✗ they both came out as "?", which
// left colour as the only difference between a passing and a failing step —
// and colour is exactly what is gone in a log, a paste, or a screenshot.
func TestASCIIGlyphsStayDistinguishable(t *testing.T) {
	st := Styles{Glyphs: ASCIIGlyphs()}
	ok := Collapsed(ToolBlock("a1", "bash", "go test ./...", "ok 1.9s", "out", false, 0), 80, st)
	bad := Collapsed(ToolBlock("a2", "bash", "go test ./...", "FAIL", "out", true, 0), 80, st)

	if ok == bad {
		t.Fatal("success and failure render identically")
	}
	for _, s := range []string{ok, bad} {
		for _, r := range s {
			if r > 0x7f {
				t.Fatalf("ASCII glyph set still emitted a non-ASCII rune %q in:\n%s", r, s)
			}
		}
	}
}

// TestGlyphSetsAgreeOnLayout guards the width budget: an ASCII glyph is a
// different number of columns ("ERR" is three, ✗ is one), and the rows still
// have to fit.
func TestGlyphSetsAgreeOnLayout(t *testing.T) {
	blocks := []Block{
		UserBlock("帮我把 glob 的 ** 支持修好"),
		ThinkingBlock("t1", "body", 90*time.Second, 25000),
		ToolBlock("a1", "bash", "go test -race ./... -count=1", "", "很长的中文输出\n第二行", false, 2*time.Second),
		ToolBlock("a2", "grep", "模式包含中文字符", "", "", true, 0),
		SystemBlock("预算已超限", true),
	}
	for _, st := range []Styles{{}, {Glyphs: ASCIIGlyphs()}, {Glyphs: UnicodeGlyphs()}} {
		for _, w := range []int{20, 24, 40, 80, 200} {
			for i, b := range blocks {
				out := Collapsed(b, w, st)
				assertFits(t, out, w)
				if strings.TrimSpace(ansi.Strip(out)) == "" && b.Kind != KindSystem {
					t.Errorf("width=%d block %d rendered empty", w, i)
				}
			}
		}
	}
}

// TestZeroStylesUsesUnicodeGlyphs keeps Styles{} a usable plain-text renderer,
// which is what every other test in this file relies on.
func TestZeroStylesUsesUnicodeGlyphs(t *testing.T) {
	out := Collapsed(ToolBlock("a1", "bash", "ls", "ok", "out", false, 0), 80, Styles{})
	if !strings.Contains(out, UnicodeGlyphs().OK) {
		t.Fatalf("the zero Styles value did not use the Unicode glyphs:\n%s", out)
	}
}
