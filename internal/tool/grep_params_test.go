package tool

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepIgnoreCase(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"a.txt": "Hello World\n"})
		if res := grep(t, dir, Input{"pattern": "hello world"}); strings.Contains(res.Data, "a.txt") {
			t.Fatalf("case-sensitive search matched: %q", res.Data)
		}
		res := grep(t, dir, Input{"pattern": "hello world", "ignore_case": true})
		if res.IsError || !strings.Contains(res.Data, "a.txt:1:Hello World") {
			t.Fatalf("ignore_case grep = %q", res.Data)
		}
	})
}

func TestGrepContextLines(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"a.txt": "one\ntwo\nNEEDLE\nfour\nfive\n"})
		res := grep(t, dir, Input{"pattern": "NEEDLE", "context": float64(1)})
		if res.IsError {
			t.Fatalf("grep failed: %s", res.Data)
		}
		for _, want := range []string{"a.txt-2-two", "a.txt:3:NEEDLE", "a.txt-4-four"} {
			if !strings.Contains(res.Data, want) {
				t.Errorf("context output lacks %q: %q", want, res.Data)
			}
		}
		for _, unwanted := range []string{"one", "five"} {
			if strings.Contains(res.Data, unwanted) {
				t.Errorf("context=1 output has %q: %q", unwanted, res.Data)
			}
		}
	})
}

func TestGrepFilesOnly(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"a.txt": "needle\nneedle\n", "sub/b.txt": "x\nneedle\n", "c.txt": "nothing\n"})
		res := grep(t, dir, Input{"pattern": "needle", "files_only": true})
		if res.IsError {
			t.Fatalf("grep failed: %s", res.Data)
		}
		got := strings.Split(strings.TrimSpace(res.Data), "\n")
		want := map[string]bool{"a.txt": true, filepath.Join("sub", "b.txt"): true}
		if len(got) != len(want) {
			t.Fatalf("files_only = %q, want exactly %v", res.Data, want)
		}
		for _, line := range got {
			if !want[strings.TrimSpace(line)] {
				t.Errorf("unexpected files_only line %q", line)
			}
		}
	})
}

// Paths are shown relative to the working directory — shorter, and what the
// other tools accept — even when the search was narrowed to a subdirectory.
func TestGrepPathsRelativeToCwd(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"sub/x.go": "package sub // needle\n"})
		rel := filepath.Join("sub", "x.go") + ":1:"
		for _, in := range []Input{{"pattern": "needle"}, {"pattern": "needle", "path": "sub"}, {"pattern": "needle", "path": filepath.Join(dir, "sub")}} {
			res := grep(t, dir, in)
			if !strings.HasPrefix(res.Data, rel) {
				t.Errorf("grep %v = %q, want it to start with %q", in, res.Data, rel)
			}
			if strings.Contains(res.Data, dir) {
				t.Errorf("grep %v shows the absolute path: %q", in, res.Data)
			}
		}
	})
}
