package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFileTool(t *testing.T, dir string, in Input) Result {
	t.Helper()
	res, err := NewReadTool().Call(context.Background(), in, Context{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A 5000-character line (minified JS, a JSON blob) is cut to 2000 characters
// with a note of how much was left out, instead of flooding the context.
func TestReadTruncatesLongLine(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", 5000)
	if err := os.WriteFile(filepath.Join(dir, "min.js"), []byte("short\n"+long+"\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := readFileTool(t, dir, Input{"filePath": "min.js"})
	if res.IsError {
		t.Fatalf("read failed: %s", res.Data)
	}
	want := "2: " + strings.Repeat("a", 2000) + "…[+3000 chars]"
	if !strings.Contains(res.Data, want) {
		t.Fatalf("long line not cut to 2000 chars with a marker:\n%.300s", res.Data)
	}
	if strings.Contains(res.Data, strings.Repeat("a", 2001)) {
		t.Fatal("more than 2000 characters of the long line were shown")
	}
	if !strings.Contains(res.Data, "3: end") {
		t.Fatal("line after the long one is missing")
	}
}

// Multi-byte text is counted in characters, not bytes.
func TestReadTruncatesLongLineByCharacters(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("中", 2500)
	if err := os.WriteFile(filepath.Join(dir, "zh.txt"), []byte(long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := readFileTool(t, dir, Input{"filePath": "zh.txt"})
	if !strings.Contains(res.Data, "1: "+strings.Repeat("中", 2000)+"…[+500 chars]") {
		t.Fatalf("multi-byte long line not cut by characters:\n%.200s", res.Data)
	}
}

// When the line limit stops the read, the last line is a fixed marker naming
// the offset to continue from; the engine keeps that line when it truncates.
func TestReadEmitsNextOffsetMarker(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	lastLine := func(s string) string { return s[strings.LastIndexByte(s, '\n')+1:] }

	res := readFileTool(t, dir, Input{"filePath": "big.txt"})
	if got := lastLine(res.Data); got != "[next: offset=2001]" {
		t.Fatalf("last line = %q, want [next: offset=2001]", got)
	}

	res = readFileTool(t, dir, Input{"filePath": "big.txt", "offset": float64(5), "limit": float64(10)})
	if got := lastLine(res.Data); got != "[next: offset=15]" {
		t.Fatalf("last line = %q, want [next: offset=15]", got)
	}

	res = readFileTool(t, dir, Input{"filePath": "big.txt", "offset": float64(2001)})
	if strings.Contains(res.Data, "[next:") {
		t.Fatalf("read to the end still has a next marker: %q", lastLine(res.Data))
	}
	if got := lastLine(res.Data); got != "2500: line 2500" {
		t.Fatalf("last line = %q, want the file's last line", got)
	}
}
