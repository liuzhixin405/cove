package tool

import (
	"os"
	"strings"
	"testing"
)

// (a) The file is indented with 4 spaces, the model's oldString/newString with
// tabs. The whitespace-normalized match is unique, so it is applied — and the
// written text follows the file's indentation, not the model's.
func TestEditFuzzyReindentsToFileIndentation(t *testing.T) {
	original := "package main\n\nfunc f() {\n    if x {\n        y()\n    }\n}\n"
	dir, path := writeTempFile(t, original)
	oldS := "func f() {\n\tif x {\n\t\ty()\n\t}\n}"
	newS := "func f() {\n\tif x {\n\t\ty()\n\t\tz()\n\t}\n}"
	res := mustCall(t, dir, oldS, newS, false)
	if res.IsError {
		t.Fatalf("expected fuzzy match to apply, got error: %s", res.Data)
	}
	if !strings.Contains(res.Data, "fuzzy whitespace match") {
		t.Errorf("result does not say the match was fuzzy: %s", res.Data)
	}
	if !strings.Contains(res.Data, "lines 3-7") {
		t.Errorf("result does not name the matched line range 3-7: %s", res.Data)
	}
	got, _ := os.ReadFile(path)
	want := "package main\n\nfunc f() {\n    if x {\n        y()\n        z()\n    }\n}\n"
	if string(got) != want {
		t.Fatalf("file = %q\nwant  %q", got, want)
	}
}

// (b) The matched block's own indentation is inconsistent (tabs and spaces
// mixed), so a multi-line newString cannot be re-indented safely: refuse and
// tell the model to read the lines again.
func TestEditFuzzyInconsistentIndentRefusesMultiLine(t *testing.T) {
	original := "func f() {\n\tif x {\n      y()\n\t}\n}\n"
	dir, path := writeTempFile(t, original)
	oldS := "func f() {\n\tif x {\n\t\ty()\n\t}\n}"
	newS := "func f() {\n\tif x {\n\t\ty()\n\t\tz()\n\t}\n}"
	res := mustCall(t, dir, oldS, newS, false)
	if !res.IsError {
		t.Fatalf("expected an error for inconsistent indentation, got: %s", res.Data)
	}
	if !strings.Contains(res.Data, "read") {
		t.Errorf("error does not tell the model to read the file again: %s", res.Data)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("file changed on a refused edit: %q", got)
	}
}

// (c) An exact oldString found three times without replaceAll names the lines.
func TestEditExactMultipleMatchesListsLineNumbers(t *testing.T) {
	dir, _ := writeTempFile(t, "foo\nbar\nfoo\nbaz\nfoo\n")
	res := mustCall(t, dir, "foo", "qux", false)
	if !res.IsError {
		t.Fatalf("expected an error, got: %s", res.Data)
	}
	if !strings.Contains(res.Data, "lines 1, 3, 5") {
		t.Fatalf("error does not list the matching lines 1, 3, 5: %s", res.Data)
	}
}

// (d) A successful edit shows the changed region with two lines either side.
func TestEditSuccessShowsContextSnippet(t *testing.T) {
	dir, _ := writeTempFile(t, "l1\nl2\nl3\nTARGET\nl5\nl6\nl7\n")
	res := mustCall(t, dir, "TARGET", "NEW", false)
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Data)
	}
	for _, want := range []string{"2: l2", "3: l3", "4: NEW", "5: l5", "6: l6"} {
		if !strings.Contains(res.Data, want) {
			t.Errorf("snippet lacks %q:\n%s", want, res.Data)
		}
	}
	for _, unwanted := range []string{"1: l1", "7: l7"} {
		if strings.Contains(res.Data, unwanted) {
			t.Errorf("snippet has more than two lines of context (%q):\n%s", unwanted, res.Data)
		}
	}
}

// The indentation unit used to be guessed from the matched block alone. A
// block whose lines all sit at the same depth says nothing about the unit, so
// four spaces in a two-space file were taken as one level, and a deeper
// newString line got eight spaces instead of six. The unit now comes from the
// whole file.
func TestEditFuzzyUsesWholeFileIndentUnit(t *testing.T) {
	original := "root:\n  a:\n    b: 1\n    c: 2\n  d: 3\n"
	dir, path := writeTempFile(t, original)
	oldS := "\t\tb: 1\n\t\tc: 2"
	newS := "\t\tb: 1\n\t\tc: 2\n\t\t\te: 4"
	res := mustCall(t, dir, oldS, newS, false)
	if res.IsError {
		t.Fatalf("fuzzy edit refused: %s", res.Data)
	}
	got, _ := os.ReadFile(path)
	want := "root:\n  a:\n    b: 1\n    c: 2\n      e: 4\n  d: 3\n"
	if string(got) != want {
		t.Fatalf("file = %q\nwant  %q", got, want)
	}
}

func TestFileIndentUnit(t *testing.T) {
	cases := []struct {
		src  string
		want int
	}{
		{"a\n  b\n    c\n  d\n", 2},
		{"a\n    b\n        c\n    d\n      continuation\n", 4},
		{"a\n\tb\n", 0},
		{"no indentation\n", 0},
	}
	for _, c := range cases {
		if got := fileIndentUnit(strings.Split(c.src, "\n")); got != c.want {
			t.Errorf("fileIndentUnit(%q) = %d, want %d", c.src, got, c.want)
		}
	}
}

// Fix round 1 (item 3): the whole-file unit must not override what the block
// itself says. A single-depth block whose depth the file unit does not map
// onto the model's depth keeps the block's own guess (8 spaces = 2 levels of
// 4, model 1 tab → the new deeper line gets 12, not 10).
func TestEditFuzzySingleDepthBlockKeepsOwnUnit(t *testing.T) {
	original := "a:\n  b:\n    c:\n      d:\n        x: 1\n        y: 2\n  e: 3\n"
	dir, path := writeTempFile(t, original)
	res := mustCall(t, dir, "\tx: 1\n\ty: 2", "\tx: 1\n\ty: 2\n\t\tz: 3", false)
	if res.IsError {
		t.Fatalf("fuzzy edit refused: %s", res.Data)
	}
	got, _ := os.ReadFile(path)
	want := "a:\n  b:\n    c:\n      d:\n        x: 1\n        y: 2\n            z: 3\n  e: 3\n"
	if string(got) != want {
		t.Fatalf("file = %q\nwant  %q", got, want)
	}
}

// A block with two depths carries its own unit (8 and 12 → 4), which wins
// over the file's 2.
func TestEditFuzzyMultiDepthBlockUsesOwnUnit(t *testing.T) {
	original := "a:\n  b:\n    c:\n      d:\n        x:\n            y: 1\n  e: 3\n"
	dir, path := writeTempFile(t, original)
	res := mustCall(t, dir, "\tx:\n\t\ty: 1", "\tx:\n\t\ty: 1\n\t\t\tz: 2", false)
	if res.IsError {
		t.Fatalf("fuzzy edit refused: %s", res.Data)
	}
	got, _ := os.ReadFile(path)
	want := "a:\n  b:\n    c:\n      d:\n        x:\n            y: 1\n                z: 2\n  e: 3\n"
	if string(got) != want {
		t.Fatalf("file = %q\nwant  %q", got, want)
	}
}
