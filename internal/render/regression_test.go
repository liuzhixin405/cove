package render

import (
	"strings"
	"testing"
)

// A hunk that begins with an addition had its new-side start overwritten by
// the first context line after it: prepending a line gave "+2,3" instead of
// "+1,3". The expected headers are what diff -U3 prints.
func TestDiffHunkHeaders(t *testing.T) {
	lines := func(n int) string {
		var sb strings.Builder
		for i := 1; i <= n; i++ {
			sb.WriteString(string(rune('0'+i%10)) + "\n")
		}
		return sb.String()
	}
	ten := lines(10)
	tenWithX := strings.Replace(ten, "6\n", "6\nx\n", 1)
	for _, tc := range []struct {
		name, old, new, header string
	}{
		{"prepend", "a\nb\n", "x\na\nb\n", "@@ -1,2 +1,3 @@"},
		{"prepend long", ten, "x\n" + ten, "@@ -1,3 +1,4 @@"},
		{"append", "a\nb\n", "a\nb\nx\n", "@@ -1,2 +1,3 @@"},
		{"insert", "a\nb\n", "a\nx\nb\n", "@@ -1,2 +1,3 @@"},
		{"insert deep", ten, tenWithX, "@@ -4,6 +4,7 @@"},
		{"delete first", "x\na\nb\n", "a\nb\n", "@@ -1,3 +1,2 @@"},
		{"delete deep", tenWithX, ten, "@@ -4,7 +4,6 @@"},
		{"delete all", "a\nb\n", "", "@@ -1,2 +0,0 @@"},
		{"create", "", "a\nb\n", "@@ -0,0 +1,2 @@"},
	} {
		d := Diff(tc.old, tc.new)
		if got := strings.SplitN(d.Text, "\n", 2)[0]; got != tc.header {
			t.Errorf("%s: header %q, want %q\n%s", tc.name, got, tc.header, d.Text)
		}
	}
}

// A character cut in two by the chunk boundary lost its continuation bytes:
// the lead byte was printed on its own and 0x80–0x9F at the start of the
// next chunk were dropped as C1 controls, so "文件" came out as "\xe6件".
func TestStreamSanitizerJoinsARuneSplitAcrossChunks(t *testing.T) {
	s := "文件 ok"
	for cut := 1; cut < len(s); cut++ {
		var z StreamSanitizer
		got := z.Write(s[:cut]) + z.Write(s[cut:]) + z.Flush()
		if got != s {
			t.Errorf("cut %d: got %q, want %q", cut, got, s)
		}
	}
	// Byte by byte, too.
	var z StreamSanitizer
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		sb.WriteString(z.Write(s[i : i+1]))
	}
	if got := sb.String(); got != s {
		t.Errorf("byte by byte: got %q", got)
	}
}

// A stream that ends inside a character shows a replacement glyph for it on
// Flush rather than dropping it or leaving a raw byte.
func TestStreamSanitizerFlushReplacesAnUnfinishedRune(t *testing.T) {
	var z StreamSanitizer
	got := z.Write("ab" + "文"[:2])
	if got != "ab" {
		t.Fatalf("Write = %q", got)
	}
	if got := z.Flush(); got != "�" {
		t.Fatalf("Flush = %q", got)
	}
	if got := z.Flush(); got != "" {
		t.Fatalf("second Flush = %q", got)
	}
	// An unfinished escape sequence is still dropped.
	z.Write("x\x1b[3")
	if got := z.Flush(); got != "" {
		t.Fatalf("Flush of a partial CSI = %q", got)
	}
}

// Lines were split after normalising CRLF and trimming the final newline, so
// a change of only the trailing newline or of the line endings produced an
// empty diff, and the permission prompt called the write "内容不变".
func TestDiffReportsTrailingNewlineAndLineEndingChanges(t *testing.T) {
	const noEOL = `\ No newline at end of file`
	for _, tc := range []struct{ name, old, new, want string }{
		{"newline removed", "a\n", "a", "@@ -1,1 +1,1 @@\n-a\n+a\n" + noEOL},
		{"newline added", "a", "a\n", "@@ -1,1 +1,1 @@\n-a\n" + noEOL + "\n+a"},
		{"both lack it", "x\ny", "x\nz", "@@ -1,2 +1,2 @@\n x\n-y\n" + noEOL + "\n+z\n" + noEOL},
		{"create without newline", "", "a", "@@ -0,0 +1,1 @@\n+a\n" + noEOL},
	} {
		d := Diff(tc.old, tc.new)
		if d.Text != tc.want {
			t.Errorf("%s: got\n%s\nwant\n%s", tc.name, d.Text, tc.want)
		}
	}
	if d := Diff("a\nb", "a\nb"); d.Text != "" {
		t.Errorf("equal texts without a final newline: %q", d.Text)
	}
	if d := Diff("a\nb\n", "a\nb\nc"); d.Added != 1 || d.Removed != 0 || strings.Contains(d.Text, noEOL+"\n-") {
		t.Errorf("append without newline: +%d -%d\n%s", d.Added, d.Removed, d.Text)
	}

	for _, tc := range []struct{ name, old, new, note string }{
		{"CRLF to LF", "a\r\nb\r\n", "a\nb\n", "CRLF → LF"},
		{"LF to CRLF", "a\nb\n", "a\r\nb\r\n", "LF → CRLF"},
		{"mixed", "a\r\nb\n", "a\nb\r\n", "mixed"},
	} {
		d := Diff(tc.old, tc.new)
		if d.Text == "" || !strings.Contains(d.Text, tc.note) || !strings.HasPrefix(d.Text, `\ `) {
			t.Errorf("%s: line-ending change not reported: %q", tc.name, d.Text)
		}
	}
	// A content change alongside a line-ending change shows both.
	d := Diff("a\r\nb\r\n", "a\nc\n")
	for _, want := range []string{"-b", "+c", "CRLF → LF"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("mixed change lacks %q:\n%s", want, d.Text)
		}
	}
	// Converting only the changed lines is not a line-ending change.
	if d := Diff("a\r\nb\r\n", "a\r\nc\r\n"); strings.Contains(d.Text, "Line endings") {
		t.Errorf("unchanged CRLF reported as changed:\n%s", d.Text)
	}
}
