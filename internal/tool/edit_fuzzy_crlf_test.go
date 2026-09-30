package tool

import (
	"os"
	"strings"
	"testing"
)

// The fuzzy path measured the last matched line with its "\r" (lines from
// splitLinesWithWidths keep it on CRLF files), so an oldString without a
// trailing newline replaced that "\r" too and the file came out with one LF
// line in the middle of CRLF ones.
func TestEditFuzzyKeepsCRLFOfLastLine(t *testing.T) {
	cases := []struct{ name, original, oldS, newS, want string }{
		{"single line", "a\r\n  foo  bar\r\nc\r\n", "foo bar", "baz", "a\r\n  baz\r\nc\r\n"},
		{"multi line", "a\r\n  foo  bar\r\n  x   y\r\nc\r\n", "  foo bar\n  x y", "  one\n  two", "a\r\n  one\r\n  two\r\nc\r\n"},
		{"trailing newline", "a\r\n  foo  bar\r\nc\r\n", "  foo bar\n", "  baz\n", "a\r\n  baz\r\nc\r\n"},
		{"last line of file", "a\r\n  foo  bar", "foo bar", "baz", "a\r\n  baz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := writeTempFile(t, tc.original)
			res := mustCall(t, dir, tc.oldS, tc.newS, false)
			if res.IsError {
				t.Fatalf("edit failed: %s", res.Data)
			}
			if !strings.Contains(res.Data, "fuzzy") {
				t.Fatalf("expected the fuzzy path: %s", res.Data)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.want {
				t.Fatalf("file = %q\nwant  %q", got, tc.want)
			}
		})
	}
}
