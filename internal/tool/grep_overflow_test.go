package tool

import (
	"fmt"
	"strings"
	"testing"
)

func numberedLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("f.go:%d:x", i+1)
	}
	return lines
}

// rg returns every matching line (up to its per-file cap), so the overflow
// note says how many were left out instead of a bare "more matches".
func TestGrepResultCountsOverflowFromRipgrep(t *testing.T) {
	out := grepResult(numberedLines(grepMaxLines+37), true, grepOptions{})
	if !strings.Contains(out, "... at least 37 more matches not shown") {
		t.Fatalf("overflow note lacks the count:\n%s", out[len(out)-120:])
	}
	if strings.Count(out, "\n") != grepMaxLines {
		t.Fatalf("result has %d lines, want %d shown + note", strings.Count(out, "\n")+1, grepMaxLines)
	}
	files := grepResult(numberedLines(grepMaxLines+2), true, grepOptions{filesOnly: true})
	if !strings.Contains(files, "... at least 2 more files not shown") {
		t.Fatalf("files_only overflow note:\n%s", files[len(files)-80:])
	}
	ctx := grepResult(numberedLines(grepMaxLines+5), true, grepOptions{context: 2})
	if !strings.Contains(ctx, "... at least 5 more lines not shown") {
		t.Fatalf("context overflow note:\n%s", ctx[len(ctx)-80:])
	}
}

// The built-in search stops collecting at grepMaxLines+1, so it has no count.
func TestGrepResultBuiltinOverflowHasNoCount(t *testing.T) {
	out := grepResult(numberedLines(grepMaxLines+1), false, grepOptions{})
	if !strings.Contains(out, "... more matches not shown") {
		t.Fatalf("overflow note:\n%s", out[len(out)-120:])
	}
	if got := grepResult(numberedLines(3), true, grepOptions{}); strings.Contains(got, "not shown") {
		t.Fatalf("a short result has an overflow note: %q", got)
	}
	if got := grepResult(nil, true, grepOptions{}); got != "No matches found" {
		t.Fatalf("empty result = %q", got)
	}
}

// Fix round 1 (item 4): rg stops each file at --max-count, so when one file
// reached it the count is only a lower bound, and the note says why.
func TestGrepResultNamesPerFileCap(t *testing.T) {
	lines := numberedLines(grepMaxLines + 1) // one file, grepMaxLines+1 matches: capped
	lines = append(lines, "g.go:1:y")
	out := grepResult(lines, true, grepOptions{})
	if !strings.Contains(out, "some files hit the per-file cap") {
		t.Fatalf("capped result lacks the cap note:\n%s", out[len(out)-160:])
	}
	spread := make([]string, 0, grepMaxLines+10)
	for i := 0; i < grepMaxLines+10; i++ {
		spread = append(spread, fmt.Sprintf("dir/f%d.go:%d:x", i, i+1))
	}
	if out := grepResult(spread, true, grepOptions{}); strings.Contains(out, "per-file cap") {
		t.Fatalf("uncapped result has the cap note:\n%s", out[len(out)-160:])
	}
}
